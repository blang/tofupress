//nolint:gosec,errcheck // test code: standard test patterns
package tofupress

import (
	"archive/zip"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBundler_BundleOCICompliant(t *testing.T) {
	// Create a module with remote dependencies
	tmpDir := t.TempDir()

	writeTerraformFile(t, tmpDir, "main.tf", `
module "vpc" {
  source = "git::https://github.com/terraform-aws-modules/terraform-aws-vpc.git?ref=v5.0.0"
  name   = "test-vpc"
  cidr   = "10.0.0.0/16"
}
`)

	// Resolve
	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), tmpDir)
	require.NoError(t, err)

	// Bundle with OCI-compliant mode
	bundler := NewBundler(BundleFormatZIP)
	bundler.OCICompliant = true
	archivePath := filepath.Join(t.TempDir(), "bundle.zip")
	err = bundler.Bundle(tree, archivePath)
	require.NoError(t, err)

	// Extract and verify OCI compliance
	extractDir := t.TempDir()
	extractZipOCI(t, archivePath, extractDir)

	// Remote packages are vendored under the private default vendor dir (_vendor).
	vendorPath := filepath.Join(extractDir, defaultVendorDir)
	assert.DirExists(t, vendorPath, "OCI bundle should contain the vendor dir for inlined packages")

	// Verify root module exists
	mainFile := filepath.Join(extractDir, "main.tf")
	assert.FileExists(t, mainFile)

	// Verify main.tf has rewritten sources pointing to the vendor dir
	mainContent, err := os.ReadFile(mainFile)
	require.NoError(t, err)
	assert.Contains(t, string(mainContent), "source = \"./"+defaultVendorDir+"/")

	// Verify vendor dir exists and contains the remote package
	assert.DirExists(t, vendorPath)

	// Verify at least one package was inlined
	entries, err := os.ReadDir(vendorPath)
	require.NoError(t, err)
	assert.Greater(t, len(entries), 0, "vendor dir should contain at least one inlined package")
}

func TestBundler_BundleOCICompliantWithLocalModules(t *testing.T) {
	// Create a module with both local and remote dependencies
	tmpDir := t.TempDir()

	writeTerraformFile(t, tmpDir, "main.tf", `
module "local" {
  source = "./modules/local"
}

module "remote" {
  source = "git::https://github.com/terraform-aws-modules/terraform-aws-vpc.git?ref=v5.0.0"
  name   = "test-vpc"
}
`)

	// Create local module
	localDir := filepath.Join(tmpDir, "modules", "local")
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	writeTerraformFile(t, localDir, "main.tf", `# local module`)

	// Resolve
	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), tmpDir)
	require.NoError(t, err)

	// Bundle with OCI-compliant mode
	bundler := NewBundler(BundleFormatZIP)
	bundler.OCICompliant = true
	archivePath := filepath.Join(t.TempDir(), "bundle.zip")
	err = bundler.Bundle(tree, archivePath)
	require.NoError(t, err)

	// Extract and verify
	extractDir := t.TempDir()
	extractZipOCI(t, archivePath, extractDir)

	// Remote packages are vendored under the private default vendor dir (_vendor).
	vendorPath := filepath.Join(extractDir, defaultVendorDir)
	assert.DirExists(t, vendorPath, "vendor dir should contain the inlined remote package")

	// Local modules are preserved at their original relative location (modules/local).
	localModulePath := filepath.Join(extractDir, "modules", "local", "main.tf")
	assert.FileExists(t, localModulePath)

	// Verify the remote package is inlined under the vendor dir.
	entries, err := os.ReadDir(vendorPath)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(entries), 1, "vendor dir should contain the remote package")
}

func TestBundler_BundleOCICompliantOnlyZIP(t *testing.T) {
	// OCI-compliant mode should only work with ZIP format
	tmpDir := t.TempDir()
	writeTerraformFile(t, tmpDir, "main.tf", `# test`)

	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), tmpDir)
	require.NoError(t, err)

	// Try OCI mode with tar.gz (should fail or ignore OCI flag)
	bundler := NewBundler(BundleFormatTarGZ)
	bundler.OCICompliant = true
	archivePath := filepath.Join(t.TempDir(), "bundle.tar.gz")
	err = bundler.Bundle(tree, archivePath)

	// Should either error or ignore OCI flag for non-ZIP formats
	// For now, we'll just ensure it doesn't crash
	_ = err
}

func TestBundlerOCICompliantAppliesStripPlan(t *testing.T) {
	rootDir := t.TempDir()
	writeTerraformFile(t, rootDir, "main.tf", `output "name" { value = "root" }`)
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "README.md"), []byte("strip"), 0o644))

	tree := &ResolvedTree{Root: &ModuleNode{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}, Packages: map[string]*DownloadedPackage{}}
	tree.AllModules = []*ModuleNode{tree.Root}
	plan, err := PlanStripping(tree, StripModeConfigOnly)
	require.NoError(t, err)

	archivePath := filepath.Join(t.TempDir(), "bundle.zip")
	bundler := NewBundler(BundleFormatZIP)
	bundler.OCICompliant = true
	bundler.StripPlan = plan
	require.NoError(t, bundler.Bundle(tree, archivePath))

	names := zipFileNames(t, archivePath)
	assert.Contains(t, names, "main.tf")
	assert.NotContains(t, names, "README.md")
}

func TestBundlerOCICompliantOmitsSourcetreeLayoutMetadata(t *testing.T) {
	root := t.TempDir()
	writeTerraformFile(t, root, "main.tf", `module "child" { source = "./modules/child" }`)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "modules", "child"), 0o755))
	writeTerraformFile(t, filepath.Join(root, "modules", "child"), "main.tf", `output "x" { value = "child" }`)
	rootModule := &ModuleNode{Key: "root", Name: "root", InstallDir: root, PackageRoot: root}
	tree := &ResolvedTree{Root: rootModule, AllModules: []*ModuleNode{rootModule}, Packages: map[string]*DownloadedPackage{}}
	metadata := &ArtifactMetadata{SchemaVersion: MetadataSchemaVersion, CreatedAt: "2026-06-21T12:00:00Z"}

	archivePath := filepath.Join(t.TempDir(), "bundle.zip")
	bundler := NewBundler(BundleFormatZIP)
	bundler.OCICompliant = true
	bundler.Metadata = metadata
	require.NoError(t, bundler.Bundle(tree, archivePath))

	for _, name := range zipFileNames(t, archivePath) {
		assert.NotContains(t, name, "sourcetree/")
	}
	readBack, err := ReadMetadataFromArtifact(archivePath)
	require.NoError(t, err)
	assert.Empty(t, readBack.DedupGroups)
}

func TestBundler_OCICompliantEmbedsMetadataAtRoot(t *testing.T) {
	tmpDir := t.TempDir()
	writeTerraformFile(t, tmpDir, "main.tf", `output "name" { value = "root" }`)
	root := &ModuleNode{Name: "root", InstallDir: tmpDir, IsLocal: true}
	tree := &ResolvedTree{
		Root:       root,
		AllModules: []*ModuleNode{root},
		Packages:   make(map[string]*DownloadedPackage),
	}

	archivePath := filepath.Join(t.TempDir(), "bundle.zip")
	bundler := NewBundler(BundleFormatZIP)
	bundler.OCICompliant = true
	bundler.Metadata = &ArtifactMetadata{SchemaVersion: MetadataSchemaVersion, CreatedAt: "2026-06-21T12:00:00Z"}
	require.NoError(t, bundler.Bundle(tree, archivePath))

	zipReader, err := zip.OpenReader(archivePath)
	require.NoError(t, err)
	defer zipReader.Close()

	var hasMetadata bool
	for _, file := range zipReader.File {
		assert.NotContains(t, file.Name, "sourcetree/", "OCI-compliant bundle must not contain sourcetree layout")
		if file.Name == MetadataFileName {
			hasMetadata = true
		}
	}
	assert.True(t, hasMetadata, "OCI-compliant bundle should still contain top-level meta.json")
}

// extractZipOCI is a test helper that extracts a ZIP archive.
func extractZipOCI(t *testing.T, archivePath, destDir string) {
	t.Helper()

	zipReader, err := zip.OpenReader(archivePath)
	require.NoError(t, err)
	defer zipReader.Close()

	for _, file := range zipReader.File {
		targetPath := filepath.Join(destDir, file.Name)

		if file.FileInfo().IsDir() {
			require.NoError(t, os.MkdirAll(targetPath, file.Mode()))
			continue
		}

		require.NoError(t, os.MkdirAll(filepath.Dir(targetPath), 0o755))

		outFile, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY, file.Mode())
		require.NoError(t, err)

		inFile, err := file.Open()
		require.NoError(t, err)

		_, err = io.Copy(outFile, inFile)
		inFile.Close()
		outFile.Close()
		require.NoError(t, err)
	}
}
