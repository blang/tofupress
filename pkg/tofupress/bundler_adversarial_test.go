//nolint:gosec,errcheck // test files use standard permissions and safe paths
package tofupress

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBundler_CanceledWritePreservesExistingArtifact(t *testing.T) {
	rootDir := t.TempDir()
	writeTerraformFile(t, rootDir, "main.tf", `output "x" { value = true }`)
	root := &ModuleNode{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}
	tree := &ResolvedTree{Root: root, AllModules: []*ModuleNode{root}, Packages: map[string]*DownloadedPackage{}}

	for _, format := range []BundleFormat{BundleFormatZIP, BundleFormatTarGZ, BundleFormatTarXZ} {
		t.Run(string(format), func(t *testing.T) {
			outputDir := t.TempDir()
			outputPath := filepath.Join(outputDir, "existing.artifact")
			require.NoError(t, os.WriteFile(outputPath, []byte("previous-good-artifact"), 0o644))
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			err := NewBundler(format).Bundle(ctx, tree, outputPath)

			require.ErrorIs(t, err, context.Canceled)
			assert.Equal(t, "previous-good-artifact", string(mustReadFile(t, outputPath)))
			temps, globErr := filepath.Glob(filepath.Join(outputDir, ".existing.artifact.tmp-*"))
			require.NoError(t, globErr)
			assert.Empty(t, temps, "failed writes must remove transaction files")
		})
	}
}

func TestBundler_WithVendorDirIsHonoredWithoutMutatingTree(t *testing.T) {
	rootDir := t.TempDir()
	packageDir := t.TempDir()
	writeTerraformFile(t, rootDir, "main.tf", `output "root" { value = true }`)
	writeTerraformFile(t, packageDir, "main.tf", `output "dependency" { value = true }`)
	root := &ModuleNode{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}
	tree := &ResolvedTree{
		Root:       root,
		AllModules: []*ModuleNode{root},
		Packages: map[string]*DownloadedPackage{
			"pkg-safe": {PackageAddr: "https://example.invalid/package.zip", LocalDir: packageDir},
		},
	}
	outputPath := filepath.Join(t.TempDir(), "bundle.zip")

	err := NewBundler(BundleFormatZIP, WithVendorDir("dependencies")).Bundle(context.Background(), tree, outputPath)
	require.NoError(t, err)

	extracted := t.TempDir()
	extractZip(t, outputPath, extracted)
	assert.FileExists(t, filepath.Join(extracted, "dependencies", "pkg-safe", "main.tf"))
	assert.Empty(t, tree.VendorDir)
}

func TestBundler_PivotRewritesOnlyStagedEntry(t *testing.T) {
	packageRoot := t.TempDir()
	entryDir := filepath.Join(packageRoot, "modules", "a")
	siblingDir := filepath.Join(packageRoot, "modules", "b")
	require.NoError(t, os.MkdirAll(entryDir, 0o755))
	require.NoError(t, os.MkdirAll(siblingDir, 0o755))
	entryConfig := `module "b" { source = "../b" }`
	writeTerraformFile(t, entryDir, "main.tf", entryConfig)
	writeTerraformFile(t, siblingDir, "main.tf", `output "b" { value = true }`)
	root := &ModuleNode{Name: "root", InstallDir: entryDir, PackageRoot: packageRoot, IsLocal: true}
	sibling := &ModuleNode{Key: "b", Name: "b", InstallDir: siblingDir, PackageRoot: packageRoot, IsLocal: true, Parent: root}
	root.Children = []*ModuleNode{sibling}
	tree := &ResolvedTree{Root: root, AllModules: []*ModuleNode{root, sibling}, Packages: map[string]*DownloadedPackage{}}
	outputPath := filepath.Join(t.TempDir(), "bundle.zip")

	err := NewBundler(BundleFormatZIP).Bundle(context.Background(), tree, outputPath)
	require.NoError(t, err)

	assert.Equal(t, entryConfig, string(mustReadFile(t, filepath.Join(entryDir, "main.tf"))))
	extracted := t.TempDir()
	extractZip(t, outputPath, extracted)
	assert.Contains(t, string(mustReadFile(t, filepath.Join(extracted, "main.tf"))), `source = "./modules/b"`)
	assert.FileExists(t, filepath.Join(extracted, "modules", "b", "main.tf"))
}

func TestBundler_RefusesPivotWhenLocalModuleTargetsPackageRoot(t *testing.T) {
	packageRoot := t.TempDir()
	entryDir := filepath.Join(packageRoot, "examples", "complete")
	require.NoError(t, os.MkdirAll(entryDir, 0o755))
	writeTerraformFile(t, entryDir, "main.tf", `module "package" { source = "../.." }`)
	writeTerraformFile(t, packageRoot, "main.tf", `output "package" { value = true }`)
	root := &ModuleNode{Name: "root", InstallDir: entryDir, PackageRoot: packageRoot, IsLocal: true}
	packageModule := &ModuleNode{Key: "package", Name: "package", InstallDir: packageRoot, PackageRoot: packageRoot, IsLocal: true, Parent: root}
	root.Children = []*ModuleNode{packageModule}
	tree := &ResolvedTree{Root: root, AllModules: []*ModuleNode{root, packageModule}, Packages: map[string]*DownloadedPackage{}}
	outputPath := filepath.Join(t.TempDir(), "bundle.zip")

	err := NewBundler(BundleFormatZIP).Bundle(context.Background(), tree, outputPath)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot preserve")
	assert.Contains(t, err.Error(), "tofupress tree")
	assert.NoFileExists(t, outputPath)
}

func TestBundler_RejectsCrossPlatformTraversingArchiveName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("backslash is a native separator on Windows")
	}
	rootDir := t.TempDir()
	unsafeName := ".." + string('\\') + "escape.tf"
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, unsafeName), []byte("secret"), 0o644))
	root := &ModuleNode{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}
	tree := &ResolvedTree{Root: root, AllModules: []*ModuleNode{root}, Packages: map[string]*DownloadedPackage{}}
	outputPath := filepath.Join(t.TempDir(), "bundle.zip")
	require.NoError(t, os.WriteFile(outputPath, []byte("previous-good-artifact"), 0o644))

	err := NewBundler(BundleFormatZIP).Bundle(context.Background(), tree, outputPath)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsafe portable archive path")
	assert.Equal(t, "previous-good-artifact", string(mustReadFile(t, outputPath)))
}

func TestStageBundle_SymlinksResolved(t *testing.T) {
	rootDir := t.TempDir()
	targetContent := []byte("real content")
	realFile := filepath.Join(rootDir, "real.tf")
	require.NoError(t, os.WriteFile(realFile, targetContent, 0o644))

	// Create symlink
	symlinkFile := filepath.Join(rootDir, "link.tf")
	require.NoError(t, os.Symlink(realFile, symlinkFile))

	tree := &ResolvedTree{
		Root:       &ModuleNode{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true},
		AllModules: []*ModuleNode{{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}},
		Packages:   map[string]*DownloadedPackage{},
	}

	stagingDir, err := stageBundle(tree, "modules", nil, nil)
	require.NoError(t, err)
	defer os.RemoveAll(stagingDir)

	// Symlink should be resolved to a real file with the target's content
	data, err := os.ReadFile(filepath.Join(stagingDir, "link.tf"))
	require.NoError(t, err)
	assert.Equal(t, targetContent, data)
}

func TestStageBundle_RejectsSymlinkOutsidePackage(t *testing.T) {
	rootDir := t.TempDir()
	outsideDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outsideDir, "secret.tf"), []byte("secret"), 0o600))
	if err := os.Symlink(filepath.Join(outsideDir, "secret.tf"), filepath.Join(rootDir, "leak.tf")); err != nil {
		t.Skip("symlinks not supported on this system")
	}
	root := &ModuleNode{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}
	tree := &ResolvedTree{Root: root, AllModules: []*ModuleNode{root}, Packages: map[string]*DownloadedPackage{}}

	stagingDir, err := stageBundle(tree, "modules", nil, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "outside package root")
	assert.Empty(t, stagingDir)
}

func TestStageBundle_RejectsSymlinkDirectoryCycle(t *testing.T) {
	rootDir := t.TempDir()
	subDir := filepath.Join(rootDir, "sub")
	require.NoError(t, os.Mkdir(subDir, 0o750))
	if err := os.Symlink("..", filepath.Join(subDir, "back")); err != nil {
		t.Skip("symlinks not supported on this system")
	}
	root := &ModuleNode{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}
	tree := &ResolvedTree{Root: root, AllModules: []*ModuleNode{root}, Packages: map[string]*DownloadedPackage{}}

	stagingDir, err := stageBundle(tree, "modules", nil, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "symlink cycle")
	assert.Empty(t, stagingDir)
}

func TestStageBundle_DoesNotDuplicateModulesOwnedByDownloadedPackage(t *testing.T) {
	rootDir := t.TempDir()
	packageDir := t.TempDir()
	helperDir := filepath.Join(packageDir, "helper")
	require.NoError(t, os.MkdirAll(helperDir, 0o755))
	writeTerraformFile(t, rootDir, "main.tf", `output "root" { value = true }`)
	writeTerraformFile(t, helperDir, "main.tf", `output "helper" { value = true }`)
	root := &ModuleNode{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}
	helper := &ModuleNode{Key: "remote.helper", Name: "helper", InstallDir: helperDir, PackageRoot: packageDir, IsLocal: true, Parent: root}
	tree := &ResolvedTree{
		Root:       root,
		AllModules: []*ModuleNode{root, helper},
		Packages: map[string]*DownloadedPackage{
			"pkg-safe": {PackageAddr: "https://example.invalid/package.zip", LocalDir: packageDir},
		},
	}

	stagingDir, err := stageBundle(tree, "modules", nil, nil)
	require.NoError(t, err)
	defer os.RemoveAll(stagingDir)

	assert.FileExists(t, filepath.Join(stagingDir, "modules", "pkg-safe", "helper", "main.tf"))
	assert.NoFileExists(t, filepath.Join(stagingDir, "remote", "helper", "main.tf"))
}

func TestStageBundle_RejectsTraversingPackageIdentity(t *testing.T) {
	rootDir := t.TempDir()
	packageDir := t.TempDir()
	writeTerraformFile(t, rootDir, "main.tf", `output "root" { value = true }`)
	writeTerraformFile(t, packageDir, "main.tf", `output "package" { value = true }`)
	root := &ModuleNode{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}
	tree := &ResolvedTree{
		Root:       root,
		AllModules: []*ModuleNode{root},
		Packages: map[string]*DownloadedPackage{
			"../../escape": {PackageAddr: "https://example.invalid/package.zip", LocalDir: packageDir},
		},
	}

	stagingDir, err := stageBundle(tree, "modules", nil, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid package identity")
	assert.Empty(t, stagingDir)
}

func TestStageBundle_RejectsTraversingModuleKey(t *testing.T) {
	rootDir := t.TempDir()
	moduleDir := t.TempDir()
	writeTerraformFile(t, rootDir, "main.tf", `output "root" { value = true }`)
	writeTerraformFile(t, moduleDir, "main.tf", `output "module" { value = true }`)
	root := &ModuleNode{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}
	module := &ModuleNode{Key: "../../escape", Name: "escape", InstallDir: moduleDir, PackageRoot: moduleDir, IsLocal: true, Parent: root}
	tree := &ResolvedTree{Root: root, AllModules: []*ModuleNode{root, module}, Packages: map[string]*DownloadedPackage{}}

	stagingDir, err := stageBundle(tree, "modules", nil, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid archive path for local module")
	assert.Empty(t, stagingDir)
}

func TestStageBundle_SpecialCharactersInFilenames(t *testing.T) {
	rootDir := t.TempDir()
	// File with spaces, unicode, shell metacharacters
	filename := "module name with spaces and émojis 🙋.tf"
	writeTerraformFile(t, rootDir, filename, `output "special" { value = "ok" }`)

	tree := &ResolvedTree{
		Root:       &ModuleNode{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true},
		AllModules: []*ModuleNode{{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}},
		Packages:   map[string]*DownloadedPackage{},
	}

	stagingDir, err := stageBundle(tree, "modules", nil, nil)
	require.NoError(t, err)
	defer os.RemoveAll(stagingDir)

	entries, err := os.ReadDir(stagingDir)
	require.NoError(t, err)
	found := false
	for _, e := range entries {
		if e.Name() == filename {
			found = true
			break
		}
	}
	assert.True(t, found, "file with special characters must be staged")
}

func TestStageBundle_EmptyRootDir(t *testing.T) {
	rootDir := t.TempDir()

	tree := &ResolvedTree{
		Root:       &ModuleNode{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true},
		AllModules: []*ModuleNode{{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}},
		Packages:   map[string]*DownloadedPackage{},
	}

	stagingDir, err := stageBundle(tree, "modules", nil, nil)
	require.NoError(t, err)
	defer os.RemoveAll(stagingDir)

	entries, err := os.ReadDir(stagingDir)
	require.NoError(t, err)
	assert.Empty(t, entries, "empty source dir should produce empty staging")
}

func TestStageBundle_CrossFilesystem(t *testing.T) {
	rootDir := t.TempDir()
	writeTerraformFile(t, rootDir, "main.tf", `output "test" { value = "cross-fs" }`)

	// Create a package on a potentially different filesystem
	pkgDir := t.TempDir()
	writeTerraformFile(t, pkgDir, "main.tf", `output "pkg" { value = "remote" }`)

	root := &ModuleNode{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}
	tree := &ResolvedTree{
		Root:       root,
		AllModules: []*ModuleNode{root},
		Packages:   map[string]*DownloadedPackage{"pkg-abc123": {PackageAddr: "git::example.com/pkg", LocalDir: pkgDir}},
	}

	stagingDir, err := stageBundle(tree, "modules", nil, nil)
	require.NoError(t, err)
	defer os.RemoveAll(stagingDir)

	assert.FileExists(t, filepath.Join(stagingDir, "main.tf"))
	assert.FileExists(t, filepath.Join(stagingDir, "modules", "pkg-abc123", "main.tf"))
}

func TestStageBundle_PreservesLegacyNamedUserDirectory(t *testing.T) {
	rootDir := t.TempDir()
	writeTerraformFile(t, rootDir, "main.tf", `output "x" { value = true }`)
	require.NoError(t, os.MkdirAll(filepath.Join(rootDir, "sourcetree"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "sourcetree", "user-data.txt"), []byte("keep"), 0o644))

	root := &ModuleNode{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}
	tree := &ResolvedTree{Root: root, AllModules: []*ModuleNode{root}, Packages: map[string]*DownloadedPackage{}}

	stagingDir, err := stageBundle(tree, defaultVendorDir, nil, nil)
	require.NoError(t, err)
	defer os.RemoveAll(stagingDir)

	assert.FileExists(t, filepath.Join(stagingDir, "sourcetree", "user-data.txt"))
}

func TestStageBundle_RejectsMetadataPathCollision(t *testing.T) {
	rootDir := t.TempDir()
	writeTerraformFile(t, rootDir, "main.tf", `output "x" { value = true }`)
	require.NoError(t, os.MkdirAll(filepath.Join(rootDir, MetadataDir), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, MetadataRelPath), []byte(`{"owner":"user"}`), 0o644))

	root := &ModuleNode{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}
	tree := &ResolvedTree{Root: root, AllModules: []*ModuleNode{root}, Packages: map[string]*DownloadedPackage{}}

	_, err := stageBundle(tree, defaultVendorDir, nil, &ArtifactMetadata{SchemaVersion: MetadataSchemaVersion})
	require.Error(t, err)
	assert.Contains(t, err.Error(), MetadataRelPath)
	assert.Contains(t, err.Error(), "reserved")
	assert.JSONEq(t, `{"owner":"user"}`, string(mustReadFile(t, filepath.Join(rootDir, MetadataRelPath))))
}

func TestValidateVendorDir(t *testing.T) {
	for _, valid := range []string{"", "_vendor", ".vendor", "terraform-modules"} {
		t.Run("valid_"+valid, func(t *testing.T) {
			require.NoError(t, ValidateVendorDir(valid))
		})
	}
	for _, invalid := range []string{".", "..", "../escape", "nested/vendor", `nested\\vendor`, ".git", ".terraform", MetadataDir, " vendor"} {
		t.Run("invalid_"+invalid, func(t *testing.T) {
			require.Error(t, ValidateVendorDir(invalid))
		})
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

func TestStageBundle_SkipsGeneratedDirs(t *testing.T) {
	rootDir := t.TempDir()
	writeTerraformFile(t, rootDir, "main.tf", `# root`)

	// Create directories that must be excluded
	require.NoError(t, os.MkdirAll(filepath.Join(rootDir, ".terraform", "modules"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, ".terraform", "state"), []byte("data"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(rootDir, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, ".git", "HEAD"), []byte("ref"), 0o644))

	tree := &ResolvedTree{
		Root:       &ModuleNode{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true},
		AllModules: []*ModuleNode{{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}},
		Packages:   map[string]*DownloadedPackage{},
	}

	stagingDir, err := stageBundle(tree, "modules", nil, nil)
	require.NoError(t, err)
	defer os.RemoveAll(stagingDir)

	assert.FileExists(t, filepath.Join(stagingDir, "main.tf"))
	assert.NoDirExists(t, filepath.Join(stagingDir, ".terraform"))
	assert.NoDirExists(t, filepath.Join(stagingDir, ".git"))
}

func TestBundler_StagedBundleMatchesContent(t *testing.T) {
	// Full tree with a local module (outside the vendor dir to avoid a vendor
	// conflict) and a remote package, bundled via staging across all formats.
	rootDir := t.TempDir()
	writeTerraformFile(t, rootDir, "main.tf", `module "local" { source = "./app" }`)

	appDir := filepath.Join(rootDir, "app")
	require.NoError(t, os.MkdirAll(appDir, 0o755))
	writeTerraformFile(t, appDir, "main.tf", `output "app" { value = "local" }`)

	// Remote package
	pkgDir := t.TempDir()
	writeTerraformFile(t, pkgDir, "main.tf", `output "remote" { value = "yes" }`)

	root := &ModuleNode{Key: "", Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}
	child := &ModuleNode{Key: "root.local", Name: "local", InstallDir: appDir, PackageRoot: rootDir, Parent: root, IsLocal: true}
	root.Children = []*ModuleNode{child}
	tree := &ResolvedTree{
		Root:       root,
		AllModules: []*ModuleNode{root, child},
		Packages:   map[string]*DownloadedPackage{"pkg-abc": {PackageAddr: "git::example.com/pkg", LocalDir: pkgDir}},
	}

	meta := &ArtifactMetadata{SchemaVersion: MetadataSchemaVersion, CreatedAt: "2026-06-24T00:00:00Z"}

	// Test all three formats
	for _, format := range []BundleFormat{BundleFormatZIP, BundleFormatTarGZ, BundleFormatTarXZ} {
		t.Run(string(format), func(t *testing.T) {
			ext := map[BundleFormat]string{BundleFormatZIP: ".zip", BundleFormatTarGZ: ".tar.gz", BundleFormatTarXZ: ".tar.xz"}[format]
			archivePath := filepath.Join(t.TempDir(), "bundle"+ext)
			bundler := NewBundler(format)
			bundler.Metadata = meta
			require.NoError(t, bundler.Bundle(context.Background(), tree, archivePath))

			// Extract and verify
			extractDir := t.TempDir()
			switch format {
			case BundleFormatZIP:
				extractZip(t, archivePath, extractDir)
			case BundleFormatTarGZ:
				extractTarGz(t, archivePath, extractDir)
			case BundleFormatTarXZ:
				extractTarXZ(t, archivePath, extractDir)
			}

			assert.FileExists(t, filepath.Join(extractDir, "main.tf"))
			assert.FileExists(t, filepath.Join(extractDir, "app", "main.tf"))
			assert.FileExists(t, filepath.Join(extractDir, defaultVendorDir, "pkg-abc", "main.tf"))
			assert.FileExists(t, filepath.Join(extractDir, MetadataDir, MetadataFileName))
		})
	}
}
