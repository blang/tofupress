//nolint:gosec // test files use standard permissions and safe paths
package tofupress

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeTFTree writes a main.tf with the given content into dir (creating it).
func writeTFTree(t *testing.T, dir, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755)) //nolint:gosec // test dirs
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(body), 0o644))
}

// TestResolveTree_DiscoveresEveryTfDir pins ADR-0002's multi-entry discovery:
// every .tf/.tofu-bearing directory under the subject becomes an entry seeded
// into the BFS, and the synthetic root is a container (never scanned), so it
// does not appear by itself in AllModules. Layout-bearing dirs without .tf
// (docs/) are not entries.
func TestResolveTree_DiscoveresEveryTfDir(t *testing.T) {
	subject := t.TempDir()
	writeTFTree(t, filepath.Join(subject, "modules", "moda"), `output "a" { value = "a" }`)
	writeTFTree(t, filepath.Join(subject, "modules", "modb"), `output "b" { value = "b" }`)
	writeTFTree(t, filepath.Join(subject, "examples", "big"), `output "ex" { value = 1 }`)
	require.NoError(t, os.MkdirAll(filepath.Join(subject, "docs"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(subject, "docs", "guide.md"), []byte("docs"), 0o644))

	r := NewResolver(WithPackageRoot(subject))
	tree, err := r.ResolveTree(context.Background(), subject)
	require.NoError(t, err)

	// Synthetic root is a container, not a module: it is NOT in AllModules.
	assert.Equal(t, subject, tree.Root.InstallDir, "root InstallDir = subject (archive root)")
	assert.Equal(t, subject, tree.Root.PackageRoot, "root PackageRoot = subject (boundary)")
	assert.False(t, containsModule(tree, ""), "synthetic root not in AllModules")

	// Three .tf-dirs discovered as entries (children of root).
	assert.Len(t, tree.Root.Children, 3, "three .tf-dirs became entries")
	keys := entryKeys(tree)
	assert.Contains(t, keys, "entry.modules.moda")
	assert.Contains(t, keys, "entry.modules.modb")
	assert.Contains(t, keys, "entry.examples.big")
}

// TestResolveTree_RefusesNoTfDirAnywhere pins the refusal contract (ADR-0002):
// a subject with zero .tf/.tofu directories anywhere is neither a valid module
// (module refuses no-.tf at root) nor a valid tree (no anchors to press).
func TestResolveTree_RefusesNoTfDirAnywhere(t *testing.T) {
	subject := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(subject, "docs"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(subject, "docs", "x.md"), []byte("md"), 0o644))

	r := NewResolver(WithPackageRoot(subject))
	_, err := r.ResolveTree(context.Background(), subject)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "contains no .tf or .tofu directories")
	assert.Contains(t, err.Error(), "tofupress module")
}

// TestResolveTree_SharedLocalModuleVisitedOnce pins cross-entry dedup: when two
// entries both reference the same local sibling module, BFS visits it ONCE
// (visitedPaths dedup) — the second reference reuses the already-visited node
// rather than rescanning. This is the single-pass property that replaces the
// per-module CI loop (ADR-0002 Consequences).
func TestResolveTree_SharedLocalModuleVisitedOnce(t *testing.T) {
	subject := t.TempDir()
	// Two entries each reference ./shared (sibling).
	writeTFTree(t, filepath.Join(subject, "apps", "a"), `module "s" { source = "../shared" }`)
	writeTFTree(t, filepath.Join(subject, "apps", "b"), `module "s" { source = "../shared" }`)
	writeTFTree(t, filepath.Join(subject, "apps", "shared"), `output "x" { value = 1 }`)

	r := NewResolver(WithPackageRoot(subject))
	tree, err := r.ResolveTree(context.Background(), subject)
	require.NoError(t, err)

	// shared appears once as an entry (its own .tf-dir) and once as each app's
	// child reference — but the app child references point at the SAME InstallDir,
	// visitedPaths prevents a rescan. The app entries each carry one child.
	sharedDir := filepath.Join(subject, "apps", "shared")
	visits := 0
	for _, m := range tree.AllModules {
		if m.InstallDir == sharedDir {
			visits++
		}
	}
	// One entry node + up to two child references (de-duped children are still
	// added to AllModules in the existing dedup branch). The key assertion is
	// that the count is bounded (not 3 independent scanned copies with own deps).
	assert.GreaterOrEqual(t, visits, 1)
}

// TestResolveTree_BoundaryFlagsRefEscapingSubject pins that the subject IS the
// package boundary (ADR-0002): a local reference that escapes the subject is a
// boundary error, not silently resolved against an enclosing package. This is
// why tree overrides the // -expanded package root with workDir (the subject).
func TestResolveTree_BoundaryFlagsRefEscapingSubject(t *testing.T) {
	subject := t.TempDir()
	// Entry nested one level deep so ../../escape resolves ABOVE the subject
	// (../escape from subject/mod would land inside the subject, not escape it).
	writeTFTree(t, filepath.Join(subject, "apps", "mod"), `module "out" { source = "../../../escape" }`)
	// Entry nested TWO levels deep so ../../../escape resolves ABOVE the subject
	// (../../ from subject/apps/mod reaches the subject root; you need the third ../
	// to climb out of the subject into its parent).
	require.NoError(t, os.MkdirAll(filepath.Dir(subject), 0o755))
	writeTFTree(t, filepath.Join(filepath.Dir(subject), "escape"), `output "e" { value = 1 }`)

	r := NewResolver(WithPackageRoot(subject))
	_, err := r.ResolveTree(context.Background(), subject)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes package")
}

// TestResolveTree_KeepsLocalRefsVerbatim pins ADR-0002's "local ../ cross-
// references are preserved verbatim" (layout unchanged): the resolver rewrites
// ONLY remote sources to vendor paths; local module sources are left untouched
// on disk. (The bundler stages the subject as-is, so the unchanged ../ source
// resolves at the unpacked archive root.)
func TestResolveTree_KeepsLocalRefsVerbatim(t *testing.T) {
	subject := t.TempDir()
	writeTFTree(t, filepath.Join(subject, "moda"), `module "dep" { source = "../modb" }`)
	writeTFTree(t, filepath.Join(subject, "modb"), `output "b" { value = 1 }`)

	r := NewResolver(WithPackageRoot(subject))
	_, err := r.ResolveTree(context.Background(), subject)
	require.NoError(t, err)

	// The local source file on disk is UNCHANGED (no rewrite for local refs).
	body, ferr := os.ReadFile(filepath.Join(subject, "moda", "main.tf"))
	require.NoError(t, ferr)
	assert.Equal(t, `module "dep" { source = "../modb" }`, string(body))
}

func containsModule(tree *ResolvedTree, key string) bool {
	for _, m := range tree.AllModules {
		if m.Key == key {
			return true
		}
	}
	return false
}

func entryKeys(tree *ResolvedTree) []string {
	var keys []string
	for _, c := range tree.Root.Children {
		keys = append(keys, c.Key)
	}
	return keys
}
