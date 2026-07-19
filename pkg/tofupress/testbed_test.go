//nolint:gosec // test files use standard permissions and safe paths in temp dirs
package tofupress

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBed_ReproduceMissingModules is an empty scaffold for reproducing the
// user-reported bug where local modules (../ references) are missing from
// the bundle.
//
// README — how to use this testbed:
//
//  1. Replace the fixture setup below with your exact directory layout
//     and source references.
//
// 2. The test will:
//   - Resolve modules from the real filesystem (using NewResolver)
//   - Apply sourcetree identity planning
//   - Bundle to ZIP
//   - List all archive entries
//
// 3. Use 'go test ./pkg/tofupress/ -run TestBed -v' to run it.
//
//  4. If the test fails because a module is missing, uncomment the
//     DumpArchive helper at the bottom to extract and inspect the ZIP.
func TestBed_ReproduceMissingModules(t *testing.T) {
	// ── Fixture: directory layout ──────────────────────────────────────
	// Edit this section to match your scenario.

	repoRoot := t.TempDir()
	rootDir := filepath.Join(repoRoot, "live", "network", "infratest")
	require.NoError(t, os.MkdirAll(rootDir, 0o755))

	// Example local module at ../shared-a (one level up from root)
	sharedA := filepath.Join(filepath.Dir(rootDir), "shared-a")
	require.NoError(t, os.MkdirAll(sharedA, 0o755))
	writeTerraformFile(t, sharedA, "main.tf", `output "a" { value = "alpha" }`)

	// Example remote module (local git repo fixture — offline-safe)
	remoteRepo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(remoteRepo, "main.tf"),
		[]byte(`output "remote" { value = "ok" }`), 0o644))
	git := func(args ...string) {
		//nolint:gosec // git on temp dir is safe
		cmd := exec.Command("git", args...)
		cmd.Dir = remoteRepo
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, string(out))
	}
	git("init")
	git("config", "user.email", "test@example.invalid")
	git("config", "user.name", "TofuPress Test")
	git("add", ".")
	git("commit", "-m", "initial")
	git("tag", "v1.0.0")
	remoteSource := "git::" + (&url.URL{Scheme: "file", Path: remoteRepo}).String() + "?ref=v1.0.0"

	// ── Root module .tf — edit sources to match your scenario ──────────
	writeTerraformFile(t, rootDir, "main.tf", fmt.Sprintf(`
// ADD YOUR MODULE BLOCKS HERE
module "shared_a" {
  source = "../shared-a"
}

module "remote_mod" {
  source = %q
}
`, remoteSource))

	// ── Resolve ────────────────────────────────────────────────────────
	resolver := NewResolver()
	resolver.PackageRoot = repoRoot
	resolver.RootDir = rootDir
	tree, err := resolver.Resolve(context.Background(), rootDir)
	require.NoError(t, err)

	// ── Identity planning ─────────────────────────────────────────────
	stripPlan, err := PlanStripping(context.Background(), tree, StripModeOptimistic)
	require.NoError(t, err)
	identityPlan, err := BuildSourcetreeIdentityPlan(context.Background(), tree, stripPlan)
	require.NoError(t, err)
	require.NoError(t, ApplySourcetreeIdentityPlan(context.Background(), tree, identityPlan))

	stripPlan, err = PlanStripping(context.Background(), tree, StripModeOptimistic)
	require.NoError(t, err)

	// ── Bundle ────────────────────────────────────────────────────────
	archivePath := filepath.Join(t.TempDir(), "bundle.zip")
	bundler := NewBundler(BundleFormatZIP)
	bundler.StripPlan = stripPlan
	require.NoError(t, bundler.Bundle(context.Background(), tree, archivePath))

	// ── Inspect archive ────────────────────────────────────────────────
	names := zipFileNames(t, archivePath)
	t.Logf("=== Archive: %d entries ===", len(names))
	for _, n := range names {
		t.Logf("  %s", n)
	}

	// ── Assertions — add your expectations here ───────────────────────
	// Example:
	//   assert.True(t, archiveContainsDir(t, names, "shared-a"),
	//     "local module shared-a must be in archive")
	//   assert.True(t, archiveContainsDir(t, names, "modules/"),
	//     "remote package must be in archive under modules/")
	_ = archiveContainsDir // suppress unused warning (helper for user)

	// Show resolved tree for debugging
	t.Logf("\n=== Resolved tree ===")
	for _, mod := range tree.AllModules {
		t.Logf("  key=%s name=%s local=%v remote=%v pkg=%s",
			mod.Key, mod.Name, mod.IsLocal, mod.IsRemote, mod.Source.PackageAddr)
	}
	t.Logf("Packages: %d", len(tree.Packages))
	for addr, pkg := range tree.Packages {
		t.Logf("  %s -> %s", addr, pkg.LocalDir)
	}
}

// ── Legibility helpers — use in your assertions ────────────────────────

// archiveContainsDir returns true if any archive entry starts with
// dirPrefix + "/" (trailing slash added for you if missing).
func archiveContainsDir(t *testing.T, names []string, dirPrefix string) bool {
	t.Helper()
	prefix := dirPrefix
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	for _, n := range names {
		if strings.HasPrefix(n, prefix) {
			return true
		}
	}
	return false
}

// DumpArchive extracts the ZIP to a temp directory and prints the full
// file tree. Call this from your test when you need to inspect contents.
func DumpArchive(t *testing.T, archivePath string) {
	t.Helper()
	dest := t.TempDir()
	//nolint:gosec // test helper — unzip on temp files is safe
	cmd := exec.Command("unzip", "-q", archivePath, "-d", dest)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "unzip: %s", string(out))

	t.Logf("Extracted to: %s", dest)
	_ = filepath.Walk(dest, func(path string, _ os.FileInfo, _ error) error {
		rel, _ := filepath.Rel(dest, path)
		t.Logf("  %s", rel)
		return nil
	})
}

// TestBed_ToyExample is a minimal self-contained test. Use this as a
// reference or copy-paste starting point.
func TestBed_ToyExample(t *testing.T) {
	t.Skip("remove this skip when you want to use the toy example")

	tmp := t.TempDir()
	writeTerraformFile(t, tmp, "main.tf", `
module "child" {
  source = "./child"
}
`)
	childDir := filepath.Join(tmp, "child")
	require.NoError(t, os.MkdirAll(childDir, 0o755))
	writeTerraformFile(t, childDir, "main.tf", `output "hello" { value = "world" }`)

	resolver := NewResolver()
	resolver.PackageRoot = tmp
	resolver.RootDir = tmp
	tree, err := resolver.Resolve(context.Background(), tmp)
	require.NoError(t, err)

	stripPlan, err := PlanStripping(context.Background(), tree, StripModeOptimistic)
	require.NoError(t, err)

	archivePath := filepath.Join(t.TempDir(), "bundle.zip")
	bundler := NewBundler(BundleFormatZIP)
	bundler.StripPlan = stripPlan
	require.NoError(t, bundler.Bundle(context.Background(), tree, archivePath))

	names := zipFileNames(t, archivePath)
	t.Logf("Archive: %v", names)
	assert.Contains(t, names, "main.tf")
	assert.Contains(t, names, "child/main.tf")
}
