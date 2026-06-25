//nolint:gosec // test files use standard permissions and safe paths
package tofupress

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeTestFile creates a file at the given path, creating parent directories if needed.
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func TestSourcetreeIDFromHash(t *testing.T) {
	id, err := SourcetreeIDFromHash("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	require.NoError(t, err)
	assert.Equal(t, "pkg-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", id)
}

func TestSourcetreeIDFromHashRejectsInvalidHash(t *testing.T) {
	_, err := SourcetreeIDFromHash("not-hex")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid package hash")
}

func TestBuildSourcetreeIdentityPlanDeduplicatesDifferentSourcesWithSameFinalContent(t *testing.T) {
	root := t.TempDir()
	pkgA := filepath.Join(root, "modules", "old-a")
	pkgB := filepath.Join(root, "modules", "old-b")
	require.NoError(t, os.MkdirAll(pkgA, 0o755))
	require.NoError(t, os.MkdirAll(pkgB, 0o755))
	writeTerraformFile(t, pkgA, "main.tf", `output "id" { value = "same" }`)
	writeTerraformFile(t, pkgB, "main.tf", `output "id" { value = "same" }`)
	writeTestFile(t, filepath.Join(pkgA, "README.md"), "different docs a")
	writeTestFile(t, filepath.Join(pkgB, "README.md"), "different docs b")

	tree := &ResolvedTree{
		Root: &ModuleNode{Key: "root", Name: "root", InstallDir: root, PackageRoot: root},
		Packages: map[string]*DownloadedPackage{
			"old-a": {PackageAddr: "git::file:///repo-a", LocalDir: pkgA, ContentHash: "download-a"},
			"old-b": {PackageAddr: "git::file:///repo-b", LocalDir: pkgB, ContentHash: "download-b"},
		},
		AllModules: []*ModuleNode{
			{Key: "root.a", Name: "a", PackageRoot: pkgA, InstallDir: pkgA, Source: ModuleSource{PackageAddr: "git::file:///repo-a"}, IsRemote: true},
			{Key: "root.b", Name: "b", PackageRoot: pkgB, InstallDir: pkgB, Source: ModuleSource{PackageAddr: "git::file:///repo-b"}, IsRemote: true},
		},
	}
	stripPlan, err := PlanStripping(tree, StripModeModuleDir)
	require.NoError(t, err)

	plan, err := BuildSourcetreeIdentityPlan(tree, stripPlan)
	require.NoError(t, err)
	require.Len(t, plan.ByFinalID, 1)
	require.Len(t, plan.DedupGroups, 1)
	assert.ElementsMatch(t, []string{"git::file:///repo-a", "git::file:///repo-b"}, plan.DedupGroups[0].PackageAddrs)
	assert.ElementsMatch(t, []string{"root.a", "root.b"}, plan.DedupGroups[0].ModuleKeys)
}

func TestBuildSourcetreeIdentityPlanDoesNotDeduplicateDifferentFinalContent(t *testing.T) {
	root := t.TempDir()
	pkgA := filepath.Join(root, "modules", "old-a")
	pkgB := filepath.Join(root, "modules", "old-b")
	require.NoError(t, os.MkdirAll(pkgA, 0o755))
	require.NoError(t, os.MkdirAll(pkgB, 0o755))
	writeTerraformFile(t, pkgA, "main.tf", `output "id" { value = "a" }`)
	writeTerraformFile(t, pkgB, "main.tf", `output "id" { value = "b" }`)

	tree := &ResolvedTree{
		Root: &ModuleNode{Key: "root", Name: "root", InstallDir: root, PackageRoot: root},
		Packages: map[string]*DownloadedPackage{
			"old-a": {PackageAddr: "git::file:///repo-a", LocalDir: pkgA, ContentHash: "download-a"},
			"old-b": {PackageAddr: "git::file:///repo-b", LocalDir: pkgB, ContentHash: "download-b"},
		},
		AllModules: []*ModuleNode{
			{Key: "root.a", Name: "a", PackageRoot: pkgA, InstallDir: pkgA, Source: ModuleSource{PackageAddr: "git::file:///repo-a"}, IsRemote: true},
			{Key: "root.b", Name: "b", PackageRoot: pkgB, InstallDir: pkgB, Source: ModuleSource{PackageAddr: "git::file:///repo-b"}, IsRemote: true},
		},
	}
	stripPlan, err := PlanStripping(tree, StripModeModuleDir)
	require.NoError(t, err)

	plan, err := BuildSourcetreeIdentityPlan(tree, stripPlan)
	require.NoError(t, err)
	assert.Len(t, plan.ByFinalID, 2)
	assert.Empty(t, plan.DedupGroups)
}

func TestSnapshotDirectoryWithStripUsesIncludedFinalContent(t *testing.T) {
	root := t.TempDir()
	writeTerraformFile(t, root, "main.tf", `output "x" { value = "kept" }`)
	writeTestFile(t, filepath.Join(root, "README.md"), "stripped")

	rootModule := &ModuleNode{Key: "root", Name: "root", InstallDir: root, PackageRoot: root}
	tree := &ResolvedTree{
		Root:       rootModule,
		AllModules: []*ModuleNode{rootModule},
		Packages:   map[string]*DownloadedPackage{},
	}
	stripPlan, err := PlanStripping(tree, StripModeModuleDir)
	require.NoError(t, err)

	snapshot, err := SnapshotDirectoryWithStrip(root, stripPlan)
	require.NoError(t, err)
	assert.Equal(t, 1, snapshot.FileCount)
	assert.Equal(t, int64(len(`output "x" { value = "kept" }`)), snapshot.TotalBytes)

	unstripped, err := SnapshotDirectory(root)
	require.NoError(t, err)
	assert.NotEqual(t, unstripped.Hash, snapshot.Hash)
}

func TestApplySourcetreeIdentityPlanMaterializesCanonicalDirectoryAndRewritesSources(t *testing.T) {
	root := t.TempDir()
	writeTerraformFile(t, root, "main.tf", `
module "a" { source = "./modules/old-a" }
module "b" { source = "./modules/old-b" }
`)
	pkgA := filepath.Join(root, "modules", "old-a")
	pkgB := filepath.Join(root, "modules", "old-b")
	require.NoError(t, os.MkdirAll(pkgA, 0o755))
	require.NoError(t, os.MkdirAll(pkgB, 0o755))
	writeTerraformFile(t, pkgA, "main.tf", `output "id" { value = "same" }`)
	writeTerraformFile(t, pkgB, "main.tf", `output "id" { value = "same" }`)

	modA := &ModuleNode{Key: "root.a", Name: "a", PackageRoot: pkgA, InstallDir: pkgA, Source: ModuleSource{PackageAddr: "git::file:///repo-a", SubDir: ""}, IsRemote: true}
	modB := &ModuleNode{Key: "root.b", Name: "b", PackageRoot: pkgB, InstallDir: pkgB, Source: ModuleSource{PackageAddr: "git::file:///repo-b", SubDir: ""}, IsRemote: true}
	tree := &ResolvedTree{
		Root:       &ModuleNode{Key: "root", Name: "root", InstallDir: root, PackageRoot: root, Children: []*ModuleNode{modA, modB}},
		Packages:   map[string]*DownloadedPackage{"old-a": {PackageAddr: "git::file:///repo-a", LocalDir: pkgA}, "old-b": {PackageAddr: "git::file:///repo-b", LocalDir: pkgB}},
		AllModules: []*ModuleNode{modA, modB},
		VendorDir:  "modules", // this test deliberately vendors packages under modules/ (matching the .tf sources)
	}
	modA.Parent = tree.Root
	modB.Parent = tree.Root
	stripPlan, err := PlanStripping(tree, StripModeModuleDir)
	require.NoError(t, err)
	plan, err := BuildSourcetreeIdentityPlan(tree, stripPlan)
	require.NoError(t, err)
	require.Len(t, plan.ByFinalID, 1)

	err = ApplySourcetreeIdentityPlan(tree, plan)
	require.NoError(t, err)

	var finalID string
	for id := range plan.ByFinalID {
		finalID = id
	}
	assert.DirExists(t, filepath.Join(root, "modules", finalID))
	assert.NoDirExists(t, filepath.Join(root, "modules", "old-b"))
	assert.Len(t, tree.Packages, 1)
	assert.Contains(t, tree.Packages, finalID)
	assert.Equal(t, filepath.Join(root, "modules", finalID), modA.PackageRoot)
	assert.Equal(t, filepath.Join(root, "modules", finalID), modB.PackageRoot)

	mainContent, err := os.ReadFile(filepath.Join(root, "main.tf"))
	require.NoError(t, err)
	assert.Contains(t, string(mainContent), `source = "./modules/`+finalID+`"`)
	assert.NotContains(t, string(mainContent), "old-a")
	assert.NotContains(t, string(mainContent), "old-b")
}

// TestApplySourcetreeIdentityPlan_RewriterHardErrorNotSwallowed locks in the F8 fix:
// rewriteModuleSourcesToFinalIDs previously discarded every RewriteModuleSource error
// with `_ = ...`, so a read/parse/write failure would silently ship a bundle whose
// source references still pointed at the pre-rename directories. The caller must now
// surface genuine failures. Two identical packages force a dedup-into-one relocation
// so the rewriter is guaranteed to touch the malformed root main.tf.
func TestApplySourcetreeIdentityPlan_RewriterHardErrorNotSwallowed(t *testing.T) {
	root := t.TempDir()
	// Malformed HCL (missing closing brace): RewriteModuleSource cannot parse it.
	writeTestFile(t, filepath.Join(root, "main.tf"), `module "a" { source = "./modules/old-a"`)
	pkgA := filepath.Join(root, "modules", "old-a")
	pkgB := filepath.Join(root, "modules", "old-b")
	require.NoError(t, os.MkdirAll(pkgA, 0o755))
	require.NoError(t, os.MkdirAll(pkgB, 0o755))
	writeTerraformFile(t, pkgA, "main.tf", `output "id" { value = "same" }`)
	writeTerraformFile(t, pkgB, "main.tf", `output "id" { value = "same" }`)

	modA := &ModuleNode{Key: "root.a", Name: "a", PackageRoot: pkgA, InstallDir: pkgA, Source: ModuleSource{PackageAddr: "git::file:///repo-a"}, IsRemote: true}
	modB := &ModuleNode{Key: "root.b", Name: "b", PackageRoot: pkgB, InstallDir: pkgB, Source: ModuleSource{PackageAddr: "git::file:///repo-b"}, IsRemote: true}
	tree := &ResolvedTree{
		Root:       &ModuleNode{Key: "root", Name: "root", InstallDir: root, PackageRoot: root, Children: []*ModuleNode{modA, modB}},
		Packages:   map[string]*DownloadedPackage{"old-a": {PackageAddr: "git::file:///repo-a", LocalDir: pkgA}, "old-b": {PackageAddr: "git::file:///repo-b", LocalDir: pkgB}},
		AllModules: []*ModuleNode{modA, modB},
		VendorDir:  "modules",
	}
	modA.Parent = tree.Root
	modB.Parent = tree.Root

	stripPlan, err := PlanStripping(tree, StripModeModuleDir)
	require.NoError(t, err)
	plan, err := BuildSourcetreeIdentityPlan(tree, stripPlan)
	require.NoError(t, err)
	require.Len(t, plan.ByFinalID, 1, "two identical packages must dedup into one final identity")

	// The relocation forces a rewrite of the malformed root main.tf. Before the F8 fix
	// this error was silently discarded; now it must propagate.
	err = ApplySourcetreeIdentityPlan(tree, plan)
	require.Error(t, err, "rewriter parse error must not be swallowed")
	assert.Contains(t, err.Error(), "rewrite source for module a")
}
