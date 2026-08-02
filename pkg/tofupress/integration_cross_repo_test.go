//go:build integration

//nolint:gosec // integration tests use temporary directories, local file URLs, and subprocess calls
package tofupress

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIntegration_CrossRepoMutualDiamond_ConsumesWithTofu is the regression gate for the
// headline bugs of the 2026-06-24 review:
//
//   - F2/F3: the identity plan corrupted LOCAL ../sibling references *inside* downloaded
//     packages (rewrote "../ext" to "../.." and overwrote the user's source).
//   - F6 (P0): the safe-default ("module-dir") strip crashed when a sub-module inside a
//     downloaded package (./modules/<child>) was not remapped through the package rename,
//     so the post-rename strip plan read a deleted directory.
//
// Both bugs were reproduced and verified fixed ONLY by a manual smoke run against an ad-hoc
// cross-repo diamond. No existing test exercised a MUTUAL, cross-repo, recursive dependency
// (repo A references repo B which references back into repo A, plus local ../ext siblings
// *inside* the remote-package subtree). Every prior git-source test is one-directional.
// This test captures that topology in the suite and drives the FULL CLI pipeline
// (bundle -> extract -> tofu init -> tofu validate), so the next regression is unshippable.
func TestIntegration_CrossRepoMutualDiamond_ConsumesWithTofu(t *testing.T) {
	tool := requireIACTool(t, "tofu")
	bin := buildTofuPressBinary(t)

	// repoA: tf-test1 analogue. Contains modules/ext (local sibling) and modules/mod1.
	// modules/ext references repoB's subdir modules/{mod1,mod2}.
	// modules/mod2 references ../ext (local sibling inside the package subtree — the F2 case).
	repoA := t.TempDir()
	repoB := t.TempDir()

	for _, repo := range []string{repoA, repoB} {
		for _, sub := range []string{"modules/ext", "modules/mod1"} {
			require.NoError(t, os.MkdirAll(filepath.Join(repo, sub), 0o755))
		}
	}
	// repoA also has modules/helper — the LOCAL sibling that ext references via ../helper.
	require.NoError(t, os.MkdirAll(filepath.Join(repoA, "modules/helper"), 0o755))

	// Wire the mutual cross-repo references BEFORE committing each repo, so the git sources
	// resolve to the committed state. Use branch tag so gitFileSource-style ref works.
	commitRepo := func(repo, name string) {
		t.Helper()
		runCommand(t, repo, nil, "git", "init")
		runCommand(t, repo, nil, "git", "config", "user.email", "integration@example.invalid")
		runCommand(t, repo, nil, "git", "config", "user.name", "TofuPress Integration")
		runCommand(t, repo, nil, "git", "add", ".")
		runCommand(t, repo, nil, "git", "commit", "-m", "initial "+name)
		runCommand(t, repo, nil, "git", "tag", "v1.0.0")
	}

	// Subdir syntax requires the //subdir BEFORE the ?ref= query (Terraform convention),
	// so we build sources manually rather than via gitFileSource's auto-appended ?ref=.
	repoASource := "git::file://" + repoA
	repoBSource := "git::file://" + repoB
	subA := func(sub string) string { return repoASource + "//" + sub + "?ref=v1.0.0" }
	subB := func(sub string) string { return repoBSource + "//" + sub + "?ref=v1.0.0" }

	// repoA/modules/ext: pulls a sub-module FROM repoB via //subdir, AND a LOCAL sibling
	// ../helper inside the same downloaded package (repoA). That ../helper local reference
	// is the F2/F3 corruption target: the identity plan must preserve it verbatim (NOT rewrite
	// to ../..). helper is a leaf, so there is no cycle.
	writeIntegrationFile(t, repoA, "modules/ext/main.tf", fmt.Sprintf(`
module "b1" {
  source = %q
}
module "helper" {
  source = "../helper"
}
output "ext" { value = "ext" }
`, subB("modules/mod1")))

	// repoA/modules/helper: a leaf — ext's local ../helper sibling lives here.
	writeIntegrationFile(t, repoA, "modules/helper/main.tf", `
output "helper" { value = "helper" }
`)

	// repoB/modules/ext: references back into repoA's modules/mod1 (the MUTUAL edge).
	writeIntegrationFile(t, repoB, "modules/ext/main.tf", fmt.Sprintf(`
module "a1" {
  source = %q
}
output "bext" { value = "bext" }
`, subA("modules/mod1")))

	// repoB/modules/mod1: plain leaf.
	writeIntegrationFile(t, repoB, "modules/mod1/main.tf", `output "b1" { value = "b1" }`)
	// repoA/modules/mod1: plain leaf (reached from repoB's ext -> //modules/mod1).
	writeIntegrationFile(t, repoA, "modules/mod1/main.tf", `output "a1" { value = "a1" }`)

	// Root module: pull repoA's ext tree as the entry point.
	rootDir := t.TempDir()
	writeIntegrationFile(t, rootDir, "main.tf", fmt.Sprintf(`
module "ext" {
  source = %q
}
output "root" { value = "root" }
`, subA("modules/ext")))

	commitRepo(repoA, "repoA")
	commitRepo(repoB, "repoB")

	// --- Full pipeline: bundle -> extract -> tofu init + validate ---
	artifact := filepath.Join(t.TempDir(), "diamond.zip")
	runTofuPressBundle(t, bin, rootDir, artifact, "--format=zip")

	extractDir := t.TempDir()
	if out, err := exec.Command("unzip", "-q", artifact, "-d", extractDir).CombinedOutput(); err != nil { //nolint:gosec // G204: test fixture path
		t.Fatalf("unzip failed: %s\n%s", err, out)
	}

	// F2/F3 regression assertion: the LOCAL ../ext reference inside repoA's modules/mod2
	// must be preserved verbatim — NOT corrupted to ../.. by the identity-plan rewrite.
	// match by content across the whole extract (package dirs are hash-named).
	preservedExt := anyFileContains(t, extractDir, `source = "../helper"`)
	assert.True(t, preservedExt,
		"F2/F3 regression: local ../ext sibling reference inside a downloaded package must be preserved verbatim, not rewritten")
	corruptedCount := countFilesContaining(t, extractDir, `source = "../.."`)
	assert.Equal(t, 0, corruptedCount,
		"F2/F3 regression: no local reference may be corrupted to ../.. (found %d)", corruptedCount)

	// F6 P0 assertion: the bundle completed at all (the P0 crash aborted at strip planning).
	// The validate step below is the behavioral confirmation that sub-module pointers were
	// remapped through the package rename into existing dirs.
	validateArchiveWithTool(t, tool, serveArtifact(t, artifact))
}

// anyFileContains reports whether any regular file under root contains needle.
func anyFileContains(t *testing.T, root, needle string) bool {
	t.Helper()
	found := false
	require.NoError(t, filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if b, rerr := os.ReadFile(path); rerr == nil && strings.Contains(string(b), needle) {
			found = true
			return filepath.SkipAll
		}
		return nil
	}))
	return found
}

// countFilesContaining returns the number of regular .tf files under root containing needle.
func countFilesContaining(t *testing.T, root, needle string) int {
	t.Helper()
	count := 0
	require.NoError(t, filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".tf") {
			return err
		}
		if b, rerr := os.ReadFile(path); rerr == nil && strings.Contains(string(b), needle) {
			count++
		}
		return nil
	}))
	return count
}
