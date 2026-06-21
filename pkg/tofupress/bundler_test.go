//nolint:gosec // test files use standard permissions and safe paths
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
	require.NoError(t, os.MkdirAll(localDir, 0o755)) //nolint:gosec // G301: test needs standard directory permissions
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
	require.NoError(t, os.Chmod(scriptPath, 0o755)) //nolint:gosec // G302: test needs executable permissions

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
	assert.True(t, info.Mode()&0o111 != 0, "script should be executable")
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
	require.NoError(t, os.MkdirAll(level1Dir, 0o755)) //nolint:gosec // G301: test needs standard directory permissions
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
	require.NoError(t, os.MkdirAll(localDir, 0o755)) //nolint:gosec // G301: test needs standard directory permissions
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
	require.NoError(t, os.MkdirAll(localDir, 0o755)) //nolint:gosec // G301: test needs standard directory permissions
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

func TestBundler_EmbedsMetadataInZip(t *testing.T) {
	tmpDir := t.TempDir()
	writeTerraformFile(t, tmpDir, "main.tf", `output "name" { value = "root" }`)
	root := &ModuleNode{Name: "root", InstallDir: tmpDir, IsLocal: true}
	tree := &ResolvedTree{
		Root:       root,
		AllModules: []*ModuleNode{root},
		Packages:   make(map[string]*DownloadedPackage),
	}
	metadata := &ArtifactMetadata{SchemaVersion: MetadataSchemaVersion, CreatedAt: "2026-06-21T12:00:00Z"}

	archivePath := filepath.Join(t.TempDir(), "bundle.zip")
	bundler := NewBundler(BundleFormatZIP)
	bundler.Metadata = metadata
	require.NoError(t, bundler.Bundle(tree, archivePath))

	zipReader, err := zip.OpenReader(archivePath)
	require.NoError(t, err)
	defer func() { _ = zipReader.Close() }()

	var found bool
	for _, file := range zipReader.File {
		if file.Name != MetadataFileName {
			continue
		}
		found = true
		reader, err := file.Open()
		require.NoError(t, err)
		data, err := io.ReadAll(reader)
		require.NoError(t, err)
		require.NoError(t, reader.Close())
		assert.Contains(t, string(data), `"schema_version": "1"`)
	}
	assert.True(t, found, "zip bundle should contain meta.json at artifact root")
}

func TestBundler_EmbedsMetadataInTarGz(t *testing.T) {
	tmpDir := t.TempDir()
	writeTerraformFile(t, tmpDir, "main.tf", `output "name" { value = "root" }`)
	root := &ModuleNode{Name: "root", InstallDir: tmpDir, IsLocal: true}
	tree := &ResolvedTree{
		Root:       root,
		AllModules: []*ModuleNode{root},
		Packages:   make(map[string]*DownloadedPackage),
	}
	metadata := &ArtifactMetadata{SchemaVersion: MetadataSchemaVersion, CreatedAt: "2026-06-21T12:00:00Z"}

	archivePath := filepath.Join(t.TempDir(), "bundle.tar.gz")
	bundler := NewBundler(BundleFormatTarGZ)
	bundler.Metadata = metadata
	require.NoError(t, bundler.Bundle(tree, archivePath))

	extractDir := t.TempDir()
	extractTarGz(t, archivePath, extractDir)
	data, err := os.ReadFile(filepath.Join(extractDir, MetadataFileName))
	require.NoError(t, err)
	assert.Contains(t, string(data), `"schema_version": "1"`)
}

func TestDetectFormatFromPath(t *testing.T) {
	tests := []struct {
		path     string
		expected BundleFormat
		detected bool
	}{
		{"bundle.zip", BundleFormatZIP, true},
		{"/path/to/bundle.ZIP", BundleFormatZIP, true},
		{"output.tar.gz", BundleFormatTarGZ, true},
		{"output.tgz", BundleFormatTarGZ, true},
		{"output.TAR.GZ", BundleFormatTarGZ, true},
		{"output.tar.xz", BundleFormatTarXZ, true},
		{"output.txz", BundleFormatTarXZ, true},
		{"bundle.tar", "", false},
		{"bundle", "", false},
		{"bundle.rar", "", false},
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

	file, err := os.Open(archivePath) //nolint:gosec // G304: test file path is safe
	require.NoError(t, err)
	defer file.Close() //nolint:errcheck // test cleanup

	gzReader, err := gzip.NewReader(file)
	require.NoError(t, err)
	defer gzReader.Close() //nolint:errcheck // test cleanup

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
			require.NoError(t, os.MkdirAll(filepath.Dir(targetPath), 0o755))                           //nolint:gosec // G301: test needs standard directory permissions
			outFile, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY, os.FileMode(header.Mode)) //nolint:gosec // G304: test file path is safe
			require.NoError(t, err)
			_, err = io.Copy(outFile, tarReader) //nolint:gosec // G110: test doesn't need size limits
			outFile.Close()                      //nolint:errcheck // test cleanup
			require.NoError(t, err)
		}
	}
}

// extractTarXZ is a test helper that extracts a tar.xz archive.
func extractTarXZ(t *testing.T, archivePath, destDir string) {
	t.Helper()

	file, err := os.Open(archivePath) //nolint:gosec // G304: test file path is safe
	require.NoError(t, err)
	defer file.Close() //nolint:errcheck // test cleanup

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
			require.NoError(t, os.MkdirAll(filepath.Dir(targetPath), 0o755))                           //nolint:gosec // G301: test needs standard directory permissions
			outFile, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY, os.FileMode(header.Mode)) //nolint:gosec // G304: test file path is safe
			require.NoError(t, err)
			_, err = io.Copy(outFile, tarReader) //nolint:gosec // G110: test doesn't need size limits
			outFile.Close()                      //nolint:errcheck // test cleanup
			require.NoError(t, err)
		}
	}
}

// extractZip is a test helper that extracts a ZIP archive.
func extractZip(t *testing.T, archivePath, destDir string) {
	t.Helper()

	zipReader, err := zip.OpenReader(archivePath) //nolint:gosec // G304: test file path is safe
	require.NoError(t, err)
	defer zipReader.Close() //nolint:errcheck // test cleanup

	for _, file := range zipReader.File {
		targetPath := filepath.Join(destDir, file.Name)

		if file.FileInfo().IsDir() {
			require.NoError(t, os.MkdirAll(targetPath, file.Mode()))
			continue
		}

		require.NoError(t, os.MkdirAll(filepath.Dir(targetPath), 0o755)) //nolint:gosec // G301: test needs standard directory permissions

		outFile, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY, file.Mode()) //nolint:gosec // G304: test file path is safe
		require.NoError(t, err)

		inFile, err := file.Open()
		require.NoError(t, err)

		_, err = io.Copy(outFile, inFile) //nolint:gosec // G110: test doesn't need size limits
		inFile.Close()                    //nolint:errcheck // test cleanup
		outFile.Close()                   //nolint:errcheck // test cleanup
		require.NoError(t, err)
	}
}

func TestBundlerZIPAppliesModuleDirStripPlanWithoutMutatingSource(t *testing.T) {
	rootDir := t.TempDir()
	writeTerraformFile(t, rootDir, "main.tf", `output "name" { value = "root" }`)
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "README.md"), []byte("must remain on disk, stripped from archive"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(rootDir, "examples"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "examples", "example.tf"), []byte("strip"), 0o644))

	tree := &ResolvedTree{Root: &ModuleNode{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}, Packages: map[string]*DownloadedPackage{}}
	tree.AllModules = []*ModuleNode{tree.Root}
	plan, err := PlanStripping(tree, StripModeConfigOnly)
	require.NoError(t, err)

	archivePath := filepath.Join(t.TempDir(), "bundle.zip")
	bundler := NewBundler(BundleFormatZIP)
	bundler.StripPlan = plan
	require.NoError(t, bundler.Bundle(tree, archivePath))

	assert.Contains(t, zipFileNames(t, archivePath), "main.tf")
	assert.NotContains(t, zipFileNames(t, archivePath), "README.md")
	assert.NotContains(t, zipFileNames(t, archivePath), "examples/example.tf")
	assert.FileExists(t, filepath.Join(rootDir, "README.md"), "source tree must not be mutated")
}

func TestBundlerTarGZAppliesModuleDirStripPlan(t *testing.T) {
	rootDir := t.TempDir()
	writeTerraformFile(t, rootDir, "main.tf", `locals { rendered = templatefile("templates/userdata.tftpl", {}) }`)
	require.NoError(t, os.MkdirAll(filepath.Join(rootDir, "templates"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "templates", "userdata.tftpl"), []byte("hello"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "README.md"), []byte("strip"), 0o644))

	tree := &ResolvedTree{Root: &ModuleNode{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}, Packages: map[string]*DownloadedPackage{}}
	tree.AllModules = []*ModuleNode{tree.Root}
	plan, err := PlanStripping(tree, StripModeModuleDir)
	require.NoError(t, err)

	archivePath := filepath.Join(t.TempDir(), "bundle.tar.gz")
	bundler := NewBundler(BundleFormatTarGZ)
	bundler.StripPlan = plan
	require.NoError(t, bundler.Bundle(tree, archivePath))

	names := tarGzFileNames(t, archivePath)
	assert.Contains(t, names, "main.tf")
	assert.Contains(t, names, "templates/userdata.tftpl")
	assert.NotContains(t, names, "README.md")
}

func TestBundlerDefaultModuleDirStrippingPreservesStaticAndDynamicRuntimeFiles(t *testing.T) {
	rootDir := t.TempDir()
	writeTerraformFile(t, rootDir, "main.tf", `variable "dynamic" { type = string }
locals {
  static_template = templatefile("templates/static.tftpl", {})
  dynamic_file    = file("templates/${var.dynamic}.txt")
}`)
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "README.md"), []byte("kept because dynamic fallback keeps package"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(rootDir, "templates"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "templates", "static.tftpl"), []byte("static"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "templates", "runtime.txt"), []byte("dynamic"), 0o644))

	root := &ModuleNode{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}
	tree := &ResolvedTree{Root: root, AllModules: []*ModuleNode{root}, Packages: map[string]*DownloadedPackage{}}
	plan, err := PlanStripping(tree, StripModeModuleDir)
	require.NoError(t, err)

	archivePath := filepath.Join(t.TempDir(), "bundle.zip")
	bundler := NewBundler(BundleFormatZIP)
	bundler.StripPlan = plan
	require.NoError(t, bundler.Bundle(tree, archivePath))

	names := zipFileNames(t, archivePath)
	assert.Contains(t, names, "main.tf")
	assert.Contains(t, names, "README.md")
	assert.Contains(t, names, "templates/static.tftpl")
	assert.Contains(t, names, "templates/runtime.txt")
}

func TestBundlerDefaultModuleDirStrippingDropsIrrelevantPackageFilesWhenReadsAreStatic(t *testing.T) {
	rootDir := t.TempDir()
	writeTerraformFile(t, rootDir, "main.tf", `locals { rendered = templatefile("templates/userdata.tftpl", {}) }`)
	require.NoError(t, os.MkdirAll(filepath.Join(rootDir, "templates"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "templates", "userdata.tftpl"), []byte("keep"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "templates", "unused.tftpl"), []byte("strip"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "README.md"), []byte("strip"), 0o644))

	tree := &ResolvedTree{Root: &ModuleNode{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}, Packages: map[string]*DownloadedPackage{}}
	tree.AllModules = []*ModuleNode{tree.Root}
	plan, err := PlanStripping(tree, StripModeModuleDir)
	require.NoError(t, err)

	archivePath := filepath.Join(t.TempDir(), "bundle.zip")
	bundler := NewBundler(BundleFormatZIP)
	bundler.StripPlan = plan
	require.NoError(t, bundler.Bundle(tree, archivePath))

	names := zipFileNames(t, archivePath)
	assert.Contains(t, names, "main.tf")
	assert.Contains(t, names, "templates/userdata.tftpl")
	assert.NotContains(t, names, "templates/unused.tftpl")
	assert.NotContains(t, names, "README.md")
}
