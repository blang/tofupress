//nolint:gosec,errcheck // test code: standard test patterns
package tofupress

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStageBundle_NilTreeReturnsError(t *testing.T) {
	_, err := stageBundle(nil, "modules", nil, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "nil")
}

func TestStageBundle_RootOnly(t *testing.T) {
	rootDir := t.TempDir()
	writeTerraformFile(t, rootDir, "main.tf", `output "name" { value = "root" }`)
	writeTerraformFile(t, rootDir, "variables.tf", `variable "env" { type = string }`)

	tree := &ResolvedTree{
		Root:       &ModuleNode{Key: "", Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true},
		AllModules: []*ModuleNode{{Key: "", Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}},
		Packages:   map[string]*DownloadedPackage{},
	}

	meta := &ArtifactMetadata{SchemaVersion: MetadataSchemaVersion, CreatedAt: "2026-06-24T00:00:00Z"}
	stagingDir, err := stageBundle(tree, "modules", nil, meta)
	require.NoError(t, err)
	defer os.RemoveAll(stagingDir)

	assert.FileExists(t, filepath.Join(stagingDir, "main.tf"))
	assert.FileExists(t, filepath.Join(stagingDir, "variables.tf"))
	assert.NoDirExists(t, filepath.Join(stagingDir, "modules"))

	data, err := os.ReadFile(filepath.Join(stagingDir, MetadataFileName))
	require.NoError(t, err)
	assert.Contains(t, string(data), `"schema_version": "1"`)
}

func TestStageBundle_LocalModulesOutsideRoot(t *testing.T) {
	repoRoot := t.TempDir()
	networkDir := filepath.Join(repoRoot, "live", "network")
	rootDir := filepath.Join(networkDir, "infratest")
	sharedDir := filepath.Join(networkDir, "shared-a")
	require.NoError(t, os.MkdirAll(rootDir, 0o755))
	require.NoError(t, os.MkdirAll(sharedDir, 0o755))

	writeTerraformFile(t, rootDir, "main.tf", `module "shared" { source = "../shared-a" }`)
	writeTerraformFile(t, sharedDir, "main.tf", `output "a" { value = "alpha" }`)

	root := &ModuleNode{Key: "", Name: "root", InstallDir: rootDir, PackageRoot: repoRoot, IsLocal: true}
	sharedMod := &ModuleNode{Key: "shared", Name: "shared", InstallDir: sharedDir, Parent: root, IsLocal: true}
	root.Children = []*ModuleNode{sharedMod}
	tree := &ResolvedTree{
		Root:       root,
		AllModules: []*ModuleNode{root, sharedMod},
		Packages:   map[string]*DownloadedPackage{},
	}

	stagingDir, err := stageBundle(tree, "modules", nil, nil)
	require.NoError(t, err)
	defer os.RemoveAll(stagingDir)

	// Item 2 (//subdir pivot): the archive root pivots from PackageRoot (repoRoot)
	// to the entry subdir (InstallDir). The entry's main.tf ships at the unpacked
	// archive root; the sibling shared-a module is staged at its package-relative
	// path (live/network/shared-a) and the entry's `../shared-a` source is
	// rewritten to `./live/network/shared-a`.
	assert.FileExists(t, filepath.Join(stagingDir, "main.tf"),
		"entry main.tf must be at the unpacked archive root (item 2 pivot)")
	assert.FileExists(t, filepath.Join(stagingDir, "live", "network", "shared-a", "main.tf"),
		"in-package sibling must be staged at its package-relative path")

	// The entry source must be rewritten to the package-relative path.
	data, err := os.ReadFile(filepath.Join(stagingDir, "main.tf"))
	require.NoError(t, err)
	assert.Contains(t, string(data), `source = "./live/network/shared-a"`,
		"entry's crossing local source must be rewritten to the package-relative path")
}

func TestStageBundle_WithPackages(t *testing.T) {
	rootDir := t.TempDir()
	writeTerraformFile(t, rootDir, "main.tf", `module "vpc" { source = "git::https://example.com/vpc.git" }`)

	pkgDir := t.TempDir()
	writeTerraformFile(t, pkgDir, "main.tf", `output "vpc_id" { value = "vpc-123" }`)
	writeTerraformFile(t, pkgDir, "README.md", `strip me`)

	root := &ModuleNode{Key: "", Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}
	vpc := &ModuleNode{Key: "root.vpc", Name: "vpc", Source: ModuleSource{PackageAddr: "git::https://example.com/vpc.git"}, InstallDir: pkgDir, PackageRoot: pkgDir, Parent: root, IsRemote: true}
	root.Children = []*ModuleNode{vpc}
	tree := &ResolvedTree{
		Root:       root,
		AllModules: []*ModuleNode{root, vpc},
		Packages:   map[string]*DownloadedPackage{"pkg-abc123": {PackageAddr: "git::https://example.com/vpc.git", LocalDir: pkgDir}},
	}

	stripPlan, err := PlanStripping(tree, StripModeModuleDir)
	require.NoError(t, err)

	stagingDir, err := stageBundle(tree, "modules", stripPlan, nil)
	require.NoError(t, err)
	defer os.RemoveAll(stagingDir)

	vendorDir := filepath.Join(stagingDir, "modules")
	assert.DirExists(t, vendorDir)
	assert.FileExists(t, filepath.Join(vendorDir, "pkg-abc123", "main.tf"))
	assert.NoFileExists(t, filepath.Join(vendorDir, "pkg-abc123", "README.md"),
		"strip plan must exclude non-TF files from package")
}

func TestStageBundle_StripPlanApplied(t *testing.T) {
	rootDir := t.TempDir()
	writeTerraformFile(t, rootDir, "main.tf", `output "name" { value = "root" }`)
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "README.md"), []byte("strip"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(rootDir, "examples"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "examples", "example.tf"), []byte("strip"), 0o644))

	tree := &ResolvedTree{
		Root:       &ModuleNode{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true},
		AllModules: []*ModuleNode{{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}},
		Packages:   map[string]*DownloadedPackage{},
	}
	plan, err := PlanStripping(tree, StripModeConfigOnly)
	require.NoError(t, err)

	stagingDir, err := stageBundle(tree, "modules", plan, nil)
	require.NoError(t, err)
	defer os.RemoveAll(stagingDir)

	assert.FileExists(t, filepath.Join(stagingDir, "main.tf"))
	assert.NoFileExists(t, filepath.Join(stagingDir, "README.md"))
	assert.NoFileExists(t, filepath.Join(stagingDir, "examples", "example.tf"))
}

func TestStageBundle_PreservesDirectoryStructure(t *testing.T) {
	rootDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(rootDir, "live", "network", "infratest"), 0o755))
	writeTerraformFile(t, filepath.Join(rootDir, "live", "network", "infratest"), "main.tf", `output "nested" { value = "deep" }`)

	tree := &ResolvedTree{
		Root:       &ModuleNode{Name: "root", InstallDir: filepath.Join(rootDir, "live", "network", "infratest"), PackageRoot: rootDir, IsLocal: true},
		AllModules: []*ModuleNode{{Name: "root", InstallDir: filepath.Join(rootDir, "live", "network", "infratest"), PackageRoot: rootDir, IsLocal: true}},
		Packages:   map[string]*DownloadedPackage{},
	}

	stagingDir, err := stageBundle(tree, "modules", nil, nil)
	require.NoError(t, err)
	defer os.RemoveAll(stagingDir)

	// Item 2: the entry at <repoRoot>//live/network/infratest pivots to the
	// unpacked archive root, so main.tf ships at the root (not under
	// live/network/infratest/). No siblings to stage in this fixture.
	assert.FileExists(t, filepath.Join(stagingDir, "main.tf"))
}

func TestStageBundle_PackageInModulesHasOwnModules(t *testing.T) {
	rootDir := t.TempDir()
	writeTerraformFile(t, rootDir, "main.tf", `# root`)

	// Package with its own modules/helper/ directory (like terraform-modules-base)
	pkgDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(pkgDir, "modules", "helper"), 0o755))
	writeTerraformFile(t, filepath.Join(pkgDir, "modules", "helper"), "main.tf", `output "helper" { value = "ok" }`)
	writeTerraformFile(t, pkgDir, "main.tf", `module "helper" { source = "./modules/helper" }`)

	childMod := &ModuleNode{
		Key: "root.child", Name: "child",
		InstallDir:  filepath.Join(pkgDir, "modules", "helper"),
		PackageRoot: pkgDir, Parent: &ModuleNode{Name: "root", InstallDir: rootDir, PackageRoot: rootDir}, IsRemote: true,
	}
	root := &ModuleNode{Key: "", Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true, Children: []*ModuleNode{childMod}}

	tree := &ResolvedTree{
		Root:       root,
		AllModules: []*ModuleNode{root, childMod},
		Packages:   map[string]*DownloadedPackage{"pkg-abc123": {PackageAddr: "git::https://example.com/base.git", LocalDir: pkgDir}},
	}

	stagingDir, err := stageBundle(tree, "modules", nil, nil)
	require.NoError(t, err)
	defer os.RemoveAll(stagingDir)

	// The package's internal modules/helper/ must exist
	expectedPath := filepath.Join(stagingDir, "modules", "pkg-abc123", "modules", "helper", "main.tf")
	assert.FileExists(t, expectedPath,
		"package-internal modules/ directory must not be skipped during staging")
}

func TestStageBundle_MetadataWritten(t *testing.T) {
	rootDir := t.TempDir()
	writeTerraformFile(t, rootDir, "main.tf", `# root`)

	tree := &ResolvedTree{
		Root:       &ModuleNode{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true},
		AllModules: []*ModuleNode{{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}},
		Packages:   map[string]*DownloadedPackage{},
	}

	meta := &ArtifactMetadata{SchemaVersion: MetadataSchemaVersion, CreatedAt: "2026-06-24T12:00:00Z"}
	stagingDir, err := stageBundle(tree, "modules", nil, meta)
	require.NoError(t, err)
	defer os.RemoveAll(stagingDir)

	data, err := os.ReadFile(filepath.Join(stagingDir, MetadataFileName))
	require.NoError(t, err)
	assert.Contains(t, string(data), `"schema_version": "1"`)
	assert.Contains(t, string(data), `"created_at": "2026-06-24T12:00:00Z"`)
}

func TestStageBundle_CustomVendorDir(t *testing.T) {
	rootDir := t.TempDir()
	writeTerraformFile(t, rootDir, "main.tf", `# root`)

	pkgDir := t.TempDir()
	writeTerraformFile(t, pkgDir, "main.tf", `output "vpc_id" { value = "vpc-123" }`)

	root := &ModuleNode{Key: "", Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}
	vpc := &ModuleNode{Key: "root.vpc", Name: "vpc", Source: ModuleSource{PackageAddr: "git::https://example.com/vpc.git"}, InstallDir: pkgDir, PackageRoot: pkgDir, Parent: root, IsRemote: true}
	root.Children = []*ModuleNode{vpc}
	tree := &ResolvedTree{
		Root:       root,
		AllModules: []*ModuleNode{root, vpc},
		Packages:   map[string]*DownloadedPackage{"pkg-abc123": {PackageAddr: "git::https://example.com/vpc.git", LocalDir: pkgDir}},
	}

	stagingDir, err := stageBundle(tree, "_vendor", nil, nil)
	require.NoError(t, err)
	defer os.RemoveAll(stagingDir)

	assert.FileExists(t, filepath.Join(stagingDir, "_vendor", "pkg-abc123", "main.tf"))
	assert.NoDirExists(t, filepath.Join(stagingDir, "modules"))
}

func TestStageBundle_NilMetadataOmitsFile(t *testing.T) {
	rootDir := t.TempDir()
	writeTerraformFile(t, rootDir, "main.tf", `# root`)

	tree := &ResolvedTree{
		Root:       &ModuleNode{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true},
		AllModules: []*ModuleNode{{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}},
		Packages:   map[string]*DownloadedPackage{},
	}

	stagingDir, err := stageBundle(tree, "modules", nil, nil)
	require.NoError(t, err)
	defer os.RemoveAll(stagingDir)

	assert.NoFileExists(t, filepath.Join(stagingDir, MetadataFileName))
}
