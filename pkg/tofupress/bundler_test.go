package tofupress

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ulikunitz/xz"
)

func TestBundler_BundleLocalModulesOnly(t *testing.T) {
	// Create a simple module structure
	tmpDir := t.TempDir()

	writeTerraformFile(t, tmpDir, "main.tf", `
module "local" {
  source = "./modules/local"
}
`)

	localDir := filepath.Join(tmpDir, "modules", "local")
	require.NoError(t, os.MkdirAll(localDir, 0755))
	writeTerraformFile(t, localDir, "main.tf", `# local module`)

	// Resolve
	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), tmpDir)
	require.NoError(t, err)

	// Bundle
	bundler := NewBundler(BundleFormatTarGZ)
	archivePath := filepath.Join(t.TempDir(), "bundle.tar.gz")
	err = bundler.Bundle(tree, archivePath)
	require.NoError(t, err)

	// Verify archive exists
	assert.FileExists(t, archivePath)

	// Extract and verify contents
	extractDir := t.TempDir()
	extractTarGz(t, archivePath, extractDir)

	// Verify main.tf exists
	mainFile := filepath.Join(extractDir, "main.tf")
	assert.FileExists(t, mainFile)

	// Verify local module exists
	localFile := filepath.Join(extractDir, "modules", "local", "main.tf")
	assert.FileExists(t, localFile)
}

func TestBundler_BundleWithRemoteModules(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

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

	// Bundle
	bundler := NewBundler(BundleFormatTarGZ)
	archivePath := filepath.Join(t.TempDir(), "bundle.tar.gz")
	err = bundler.Bundle(tree, archivePath)
	require.NoError(t, err)

	// Verify archive exists
	assert.FileExists(t, archivePath)

	// Extract and verify
	extractDir := t.TempDir()
	extractTarGz(t, archivePath, extractDir)

	// Verify main.tf exists and has rewritten source
	mainFile := filepath.Join(extractDir, "main.tf")
	assert.FileExists(t, mainFile)

	mainContent, err := os.ReadFile(mainFile)
	require.NoError(t, err)
	assert.Contains(t, string(mainContent), "source = \"./sourcetree/")

	// Verify sourcetree directory exists
	sourcetreeDir := filepath.Join(extractDir, "sourcetree")
	assert.DirExists(t, sourcetreeDir)

	// Verify at least one package was bundled
	entries, err := os.ReadDir(sourcetreeDir)
	require.NoError(t, err)
	assert.Greater(t, len(entries), 0, "sourcetree should contain at least one package")
}

func TestBundler_BundlePreservesFilePermissions(t *testing.T) {
	// Create a module with files that have different permissions
	tmpDir := t.TempDir()

	writeTerraformFile(t, tmpDir, "main.tf", `# main`)
	writeTerraformFile(t, tmpDir, "script.sh", `#!/bin/bash
echo "test"`)

	// Make script executable
	scriptPath := filepath.Join(tmpDir, "script.sh")
	require.NoError(t, os.Chmod(scriptPath, 0755))

	// Create tree manually (no modules to resolve)
	tree := &ResolvedTree{
		Root: &ModuleNode{
			Key:        "",
			Name:       "root",
			InstallDir: tmpDir,
			IsLocal:    true,
		},
		AllModules: []*ModuleNode{{
			Key:        "",
			Name:       "root",
			InstallDir: tmpDir,
			IsLocal:    true,
		}},
		Packages: make(map[string]*DownloadedPackage),
	}

	// Bundle
	bundler := NewBundler(BundleFormatTarGZ)
	archivePath := filepath.Join(t.TempDir(), "bundle.tar.gz")
	err := bundler.Bundle(tree, archivePath)
	require.NoError(t, err)

	// Extract and verify permissions
	extractDir := t.TempDir()
	extractTarGz(t, archivePath, extractDir)

	scriptFile := filepath.Join(extractDir, "script.sh")
	assert.FileExists(t, scriptFile)

	info, err := os.Stat(scriptFile)
	require.NoError(t, err)
	assert.True(t, info.Mode()&0111 != 0, "script should be executable")
}

func TestBundler_BundleEmptyModule(t *testing.T) {
	// Create an empty module
	tmpDir := t.TempDir()

	tree := &ResolvedTree{
		Root: &ModuleNode{
			Key:        "",
			Name:       "root",
			InstallDir: tmpDir,
			IsLocal:    true,
		},
		AllModules: []*ModuleNode{{
			Key:        "",
			Name:       "root",
			InstallDir: tmpDir,
			IsLocal:    true,
		}},
		Packages: make(map[string]*DownloadedPackage),
	}

	// Bundle
	bundler := NewBundler(BundleFormatTarGZ)
	archivePath := filepath.Join(t.TempDir(), "bundle.tar.gz")
	err := bundler.Bundle(tree, archivePath)
	require.NoError(t, err)

	// Verify archive exists
	assert.FileExists(t, archivePath)
}

func TestBundler_BundleInvalidTree(t *testing.T) {
	bundler := NewBundler(BundleFormatTarGZ)
	archivePath := filepath.Join(t.TempDir(), "bundle.tar.gz")

	// Bundle with nil tree
	err := bundler.Bundle(nil, archivePath)
	assert.Error(t, err)
}

func TestBundler_BundleToInvalidPath(t *testing.T) {
	tmpDir := t.TempDir()

	tree := &ResolvedTree{
		Root: &ModuleNode{
			Key:        "",
			Name:       "root",
			InstallDir: tmpDir,
			IsLocal:    true,
		},
		AllModules: []*ModuleNode{{
			Key:        "",
			Name:       "root",
			InstallDir: tmpDir,
			IsLocal:    true,
		}},
		Packages: make(map[string]*DownloadedPackage),
	}

	bundler := NewBundler(BundleFormatTarGZ)

	// Try to bundle to a non-existent directory
	err := bundler.Bundle(tree, "/nonexistent/path/bundle.tar.gz")
	assert.Error(t, err)
}

func TestBundler_BundleDeduplication(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Create a module that references the same remote module twice
	tmpDir := t.TempDir()

	writeTerraformFile(t, tmpDir, "main.tf", `
module "vpc1" {
  source = "git::https://github.com/terraform-aws-modules/terraform-aws-vpc.git?ref=v5.0.0"
  name   = "vpc1"
}

module "vpc2" {
  source = "git::https://github.com/terraform-aws-modules/terraform-aws-vpc.git?ref=v5.0.0"
  name   = "vpc2"
}
`)

	// Resolve
	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), tmpDir)
	require.NoError(t, err)

	// Bundle
	bundler := NewBundler(BundleFormatTarGZ)
	archivePath := filepath.Join(t.TempDir(), "bundle.tar.gz")
	err = bundler.Bundle(tree, archivePath)
	require.NoError(t, err)

	// Extract and verify
	extractDir := t.TempDir()
	extractTarGz(t, archivePath, extractDir)

	// Verify sourcetree has only one package (deduplicated)
	sourcetreeDir := filepath.Join(extractDir, "sourcetree")
	entries, err := os.ReadDir(sourcetreeDir)
	require.NoError(t, err)
	assert.Equal(t, 1, len(entries), "sourcetree should contain exactly one deduplicated package")
}

func TestBundler_BundleNestedModules(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Create nested structure
	tmpDir := t.TempDir()

	writeTerraformFile(t, tmpDir, "main.tf", `
module "level1" {
  source = "./level1"
}
`)

	level1Dir := filepath.Join(tmpDir, "level1")
	require.NoError(t, os.MkdirAll(level1Dir, 0755))
	writeTerraformFile(t, level1Dir, "main.tf", `
module "remote" {
  source = "git::https://github.com/terraform-aws-modules/terraform-aws-vpc.git?ref=v5.0.0"
  name   = "nested-vpc"
}
`)

	// Resolve
	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), tmpDir)
	require.NoError(t, err)

	// Bundle
	bundler := NewBundler(BundleFormatTarGZ)
	archivePath := filepath.Join(t.TempDir(), "bundle.tar.gz")
	err = bundler.Bundle(tree, archivePath)
	require.NoError(t, err)

	// Extract and verify
	extractDir := t.TempDir()
	extractTarGz(t, archivePath, extractDir)

	// Verify nested structure preserved
	level1File := filepath.Join(extractDir, "level1", "main.tf")
	assert.FileExists(t, level1File)

	// Verify remote module was bundled
	sourcetreeDir := filepath.Join(extractDir, "sourcetree")
	assert.DirExists(t, sourcetreeDir)
}

func TestNewBundler(t *testing.T) {
	bundler := NewBundler(BundleFormatTarGZ)
	assert.NotNil(t, bundler)
}

func TestBundler_BundleZIP(t *testing.T) {
	// Create a simple module structure
	tmpDir := t.TempDir()

	writeTerraformFile(t, tmpDir, "main.tf", `
module "local" {
  source = "./modules/local"
}
`)

	localDir := filepath.Join(tmpDir, "modules", "local")
	require.NoError(t, os.MkdirAll(localDir, 0755))
	writeTerraformFile(t, localDir, "main.tf", `# local module`)

	// Resolve
	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), tmpDir)
	require.NoError(t, err)

	// Bundle as ZIP
	bundler := NewBundler(BundleFormatZIP)
	archivePath := filepath.Join(t.TempDir(), "bundle.zip")
	err = bundler.Bundle(tree, archivePath)
	require.NoError(t, err)

	// Verify archive exists
	assert.FileExists(t, archivePath)

	// Extract and verify contents
	extractDir := t.TempDir()
	extractZip(t, archivePath, extractDir)

	// Verify main.tf exists
	mainFile := filepath.Join(extractDir, "main.tf")
	assert.FileExists(t, mainFile)

	// Verify local module exists
	localFile := filepath.Join(extractDir, "modules", "local", "main.tf")
	assert.FileExists(t, localFile)
}

func TestBundler_BundleTarXZ(t *testing.T) {
	// Create a simple module structure
	tmpDir := t.TempDir()

	writeTerraformFile(t, tmpDir, "main.tf", `
module "local" {
  source = "./modules/local"
}
`)

	localDir := filepath.Join(tmpDir, "modules", "local")
	require.NoError(t, os.MkdirAll(localDir, 0755))
	writeTerraformFile(t, localDir, "main.tf", `# local module`)

	// Resolve
	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), tmpDir)
	require.NoError(t, err)

	// Bundle as tar.xz
	bundler := NewBundler(BundleFormatTarXZ)
	archivePath := filepath.Join(t.TempDir(), "bundle.tar.xz")
	err = bundler.Bundle(tree, archivePath)
	require.NoError(t, err)

	// Verify archive exists
	assert.FileExists(t, archivePath)

	// Extract and verify contents
	extractDir := t.TempDir()
	extractTarXZ(t, archivePath, extractDir)

	// Verify main.tf exists
	mainFile := filepath.Join(extractDir, "main.tf")
	assert.FileExists(t, mainFile)

	// Verify local module exists
	localFile := filepath.Join(extractDir, "modules", "local", "main.tf")
	assert.FileExists(t, localFile)
}

func TestParseBundleFormat(t *testing.T) {
	tests := []struct {
		input    string
		expected BundleFormat
		wantErr  bool
	}{
		{"auto", BundleFormatAuto, false},
		{"", BundleFormatAuto, false},
		{"zip", BundleFormatZIP, false},
		{"ZIP", BundleFormatZIP, false},
		{"tar.gz", BundleFormatTarGZ, false},
		{"tgz", BundleFormatTarGZ, false},
		{"tar.xz", BundleFormatTarXZ, false},
		{"txz", BundleFormatTarXZ, false},
		{"rar", "", true},
	}

	for _, tt := range tests {
		name := tt.input
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			got, err := ParseBundleFormat(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expected, got)
			}
		})
	}
}

func TestDetectFormatFromPath(t *testing.T) {
	tests := []struct {
		path      string
		expected  BundleFormat
		detected  bool
	}{
		{"bundle.zip", BundleFormatZIP, true},
		{"/path/to/bundle.ZIP", BundleFormatZIP, true},
		{"output.tar.gz", BundleFormatTarGZ, true},
		{"output.tgz", BundleFormatTarGZ, true},
		{"output.TAR.GZ", BundleFormatTarGZ, true},
		{"output.tar.xz", BundleFormatTarXZ, true},
		{"output.txz", BundleFormatTarXZ, true},
		{"bundle.tar", BundleFormatZIP, false},
		{"bundle", BundleFormatZIP, false},
		{"bundle.rar", BundleFormatZIP, false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got, ok := DetectFormatFromPath(tt.path)
			assert.Equal(t, tt.detected, ok)
			assert.Equal(t, tt.expected, got)
		})
	}
}

// extractTarGz is a test helper that extracts a tar.gz archive.
func extractTarGz(t *testing.T, archivePath, destDir string) {
	t.Helper()

	file, err := os.Open(archivePath)
	require.NoError(t, err)
	defer file.Close()

	gzReader, err := gzip.NewReader(file)
	require.NoError(t, err)
	defer gzReader.Close()

	tarReader := tar.NewReader(gzReader)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)

		targetPath := filepath.Join(destDir, header.Name)

		switch header.Typeflag {
		case tar.TypeDir:
			require.NoError(t, os.MkdirAll(targetPath, os.FileMode(header.Mode)))
		case tar.TypeReg:
			require.NoError(t, os.MkdirAll(filepath.Dir(targetPath), 0755))
			outFile, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY, os.FileMode(header.Mode))
			require.NoError(t, err)
			_, err = io.Copy(outFile, tarReader)
			outFile.Close()
			require.NoError(t, err)
		}
	}
}

// extractTarXZ is a test helper that extracts a tar.xz archive.
func extractTarXZ(t *testing.T, archivePath, destDir string) {
	t.Helper()

	file, err := os.Open(archivePath)
	require.NoError(t, err)
	defer file.Close()

	xzReader, err := xz.NewReader(file)
	require.NoError(t, err)

	tarReader := tar.NewReader(xzReader)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)

		targetPath := filepath.Join(destDir, header.Name)

		switch header.Typeflag {
		case tar.TypeDir:
			require.NoError(t, os.MkdirAll(targetPath, os.FileMode(header.Mode)))
		case tar.TypeReg:
			require.NoError(t, os.MkdirAll(filepath.Dir(targetPath), 0755))
			outFile, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY, os.FileMode(header.Mode))
			require.NoError(t, err)
			_, err = io.Copy(outFile, tarReader)
			outFile.Close()
			require.NoError(t, err)
		}
	}
}

// extractZip is a test helper that extracts a ZIP archive.
func extractZip(t *testing.T, archivePath, destDir string) {
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

		require.NoError(t, os.MkdirAll(filepath.Dir(targetPath), 0755))

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
