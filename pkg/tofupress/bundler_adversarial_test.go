//nolint:gosec,errcheck // test files use standard permissions and safe paths
package tofupress

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
			require.NoError(t, bundler.Bundle(tree, archivePath))

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
			assert.FileExists(t, filepath.Join(extractDir, MetadataFileName))
		})
	}
}
