//go:build integration

//nolint:gosec // integration tests execute test binary, extract archives, and run tofu
package tofupress

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// consumeArchiveAtSubdirWithTool extracts artifactPath into a tempdir and runs
// `tofu init -backend=false` + `tofu validate` at a SUBDIR of the unpacked
// archive (an anchor addressable via //). This is the `tofupress tree` consume
// contract (ADR-0002): the archive root is the subject as-is (NOT a pivoted
// single module), so the consumer cds to the desired anchor subdir to validate.
func consumeArchiveAtSubdirWithTool(t *testing.T, tool iacTool, artifactPath, subdir string) {
	t.Helper()
	extractDir := t.TempDir()
	if !strings.HasSuffix(artifactPath, ".zip") {
		t.Fatalf("acceptance_tree_test only handles zip artifacts; got %s", artifactPath)
	}
	extractZip(t, artifactPath, extractDir)
	runIACTool(t, tool, filepath.Join(extractDir, subdir), "init", "-backend=false", "-input=false", "-no-color")
	runIACTool(t, tool, filepath.Join(extractDir, subdir), "validate", "-no-color")
}

// TestAcceptance_Tree_MonorepoShapePreserved pins ADR-0002's `tofupress tree`
// contract: pressing the monorepo root produces an UNPIVOTED archive whose root
// IS the subject as-is — every .tf-dir sits at its staged path, the directory
// shape is preserved, and consumers address any anchor via //. No single entry
// is pivoted to the archive root.
func TestAcceptance_Tree_MonorepoShapePreserved(t *testing.T) {
	bin := buildBinary(t)
	fixture := monorepoFixtureBase(t) // whole monorepo as the subject

	artifact := filepath.Join(t.TempDir(), "tree.zip")
	runTofuPressTree(t, bin, fixture, artifact, "--format=zip")

	entries := zipFileNames(t, artifact)
	entriesJoined := strings.Join(entries, "\n")

	// The archive root carries the subject's directory shape (apps/ infra/
	// platform/), NOT a flattened single entry. No pivot happened.
	assert.Contains(t, entriesJoined, "apps/app1/main.tf", "apps/app1 staged at subject-relative path")
	assert.Contains(t, entriesJoined, "infra/modules/network/main.tf", "network module staged at path; ../modules refs resolve verbatim")
	assert.Contains(t, entriesJoined, "platform/base/main.tf", "platform/base staged at path")
	// The entry modules are NOT pivoted to the archive root (that would be `module`).
	assert.False(t, containsEntry(entries, "main.tf"), "no single main.tf pivoted to the archive root (tree is unpivoted)")
}

// TestAcceptance_Tree_ValidatesAtAnchor pins that a consumer can run
// `tofu init && tofu validate` at any anchor subdir (//infra/environments/prod)
// of a `tree` archive, because local ../ cross-references between modules are
// preserved verbatim (layout unchanged) — the ADR-0002 property that replaces
// the synthetic-root workaround.
func TestAcceptance_Tree_ValidatesAtAnchor(t *testing.T) {
	bin := buildBinary(t)
	fixture := monorepoFixtureBase(t)

	artifact := filepath.Join(t.TempDir(), "tree.zip")
	runTofuPressTree(t, bin, fixture, artifact, "--format=zip")

	for _, tool := range requireAllIACTools(t) {
		consumeArchiveAtSubdirWithTool(t, tool, artifact, filepath.FromSlash("infra/environments/prod"))
	}
}

// TestAcceptance_Tree_LocalRefsPreservedVerbatim pins that the resolver rewrites
// ONLY remote sources; local `../modules/...` sources in a `tree` archive are
// byte-identical to the subject's originals (the layout is staged as-is).
func TestAcceptance_Tree_LocalRefsPreservedVerbatim(t *testing.T) {
	bin := buildBinary(t)
	fixture := monorepoFixtureBase(t)

	artifact := filepath.Join(t.TempDir(), "tree.zip")
	runTofuPressTree(t, bin, fixture, artifact, "--format=zip")

	extractDir := t.TempDir()
	extractZip(t, artifact, extractDir)

	// The pressed prod entry kept its ../../modules/network source unchanged.
	pressedBody, err := os.ReadFile(filepath.Join(extractDir, "infra", "environments", "prod", "main.tf"))
	require.NoError(t, err)
	origBody, err := os.ReadFile(filepath.Join(fixture, "infra", "environments", "prod", "main.tf"))
	require.NoError(t, err)
	assert.Equal(t, string(origBody), string(pressedBody),
		"local ../ refs are preserved verbatim in a tree press (no rewrite)")
}

// runTofuPressTree runs the `tofupress tree` subcommand (ADR-0002) and asserts
// it succeeded. Mirrors runTofuPressBundle (which invokes `module`).
func runTofuPressTree(t *testing.T, bin, source, output string, flags ...string) (stdout string, stderr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	args := append([]string{"tree", source, output}, flags...)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = projectRoot(t)
	cmd.Env = append(os.Environ(), "TF_IN_AUTOMATION=1")
	var outBuilder, errBuilder strings.Builder
	cmd.Stdout = &outBuilder
	cmd.Stderr = &errBuilder
	err := cmd.Run()
	require.NoError(t, err, "tofupress %s failed\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), outBuilder.String(), errBuilder.String())
	return outBuilder.String(), errBuilder.String()
}

func containsEntry(entries []string, want string) bool {
	for _, e := range entries {
		if e == want {
			return true
		}
	}
	return false
}
