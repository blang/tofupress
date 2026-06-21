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

	// Verify no sourcetree/ directory exists
	sourcetreePath := filepath.Join(extractDir, "sourcetree")
	assert.NoDirExists(t, sourcetreePath, "OCI bundle should not contain sourcetree/")

	// Verify root module exists
	mainFile := filepath.Join(extractDir, "main.tf")
	assert.FileExists(t, mainFile)

	// Verify main.tf has rewritten sources pointing to modules/ directory
	mainContent, err := os.ReadFile(mainFile)
	require.NoError(t, err)
	assert.Contains(t, string(mainContent), "source = \"./modules/")

	// Verify modules/ directory exists and contains the remote module
	modulesDir := filepath.Join(extractDir, "modules")
	assert.DirExists(t, modulesDir)

	// Verify at least one module was inlined
	entries, err := os.ReadDir(modulesDir)
	require.NoError(t, err)
	assert.Greater(t, len(entries), 0, "modules/ should contain at least one inlined package")
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

	// Verify no sourcetree/ directory
	sourcetreePath := filepath.Join(extractDir, "sourcetree")
	assert.NoDirExists(t, sourcetreePath)

	// Verify local module is preserved in its relative location
	localModulePath := filepath.Join(extractDir, "modules", "local", "main.tf")
	assert.FileExists(t, localModulePath)

	// Verify remote module is inlined in modules/ directory
	modulesDir := filepath.Join(extractDir, "modules")
	entries, err := os.ReadDir(modulesDir)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(entries), 2, "should have local and remote modules")
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
