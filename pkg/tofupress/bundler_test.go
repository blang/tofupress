//nolint:gosec // test files use standard permissions and safe paths
package tofupress

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ulikunitz/xz"
)

const testCustomVendorDir = "_vendor"

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
	assert.Contains(t, string(mainContent), "source = \"./modules/")

	// Verify sourcetree directory exists
	sourcetreeDir := filepath.Join(extractDir, "modules")
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

func TestBundlerArchiveUsesFinalContentSourcetreeIDAndDeduplicates(t *testing.T) {
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
	writeTestFile(t, filepath.Join(pkgA, "README.md"), "stripped a")
	writeTestFile(t, filepath.Join(pkgB, "README.md"), "stripped b")

	modA := &ModuleNode{Key: "root.a", Name: "a", PackageRoot: pkgA, InstallDir: pkgA, Source: ModuleSource{PackageAddr: "git::file:///repo-a"}, IsRemote: true}
	modB := &ModuleNode{Key: "root.b", Name: "b", PackageRoot: pkgB, InstallDir: pkgB, Source: ModuleSource{PackageAddr: "git::file:///repo-b"}, IsRemote: true}
	tree := &ResolvedTree{
		Root:       &ModuleNode{Key: "root", Name: "root", InstallDir: root, PackageRoot: root, Children: []*ModuleNode{modA, modB}},
		Packages:   map[string]*DownloadedPackage{"old-a": {PackageAddr: "git::file:///repo-a", LocalDir: pkgA}, "old-b": {PackageAddr: "git::file:///repo-b", LocalDir: pkgB}},
		AllModules: []*ModuleNode{modA, modB},
	}
	modA.Parent = tree.Root
	modB.Parent = tree.Root
	stripPlan, err := PlanStripping(tree, StripModeModuleDir)
	require.NoError(t, err)
	identityPlan, err := BuildSourcetreeIdentityPlan(tree, stripPlan)
	require.NoError(t, err)
	require.NoError(t, ApplySourcetreeIdentityPlan(tree, identityPlan))
	stripPlan, err = PlanStripping(tree, StripModeModuleDir)
	require.NoError(t, err)

	archivePath := filepath.Join(t.TempDir(), "bundle.zip")
	bundler := NewBundler(BundleFormatZIP)
	bundler.StripPlan = stripPlan
	require.NoError(t, bundler.Bundle(tree, archivePath))

	names := zipFileNames(t, archivePath)
	var finalID string
	for id := range identityPlan.ByFinalID {
		finalID = id
	}
	assert.Contains(t, names, "modules/"+finalID+"/main.tf")
	assert.NotContains(t, names, "modules/old-a/main.tf")
	assert.NotContains(t, names, "modules/old-b/main.tf")
	assert.NotContains(t, names, "modules/"+finalID+"/README.md")
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
	sourcetreeDir := filepath.Join(extractDir, "modules")
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

// TestBundler_LocalModulesOutsideRootWithRemote_AllIncluded reproduces the user-reported
// bug: when a root module references both remote modules AND local modules via ../ paths
// (outside the root module directory but within the package boundary), the local ../
// modules were not being included in the bundle.
func TestBundler_LocalModulesOutsideRootWithRemote_AllIncluded(t *testing.T) {
	// Layout mimics a real repo:
	//   repo/
	//     live/network/
	//       shared-a/           ← local module at ../shared-a (one level up)
	//       shared-b/           ← local module at ../shared-b
	//       shared-c/           ← local module at ../shared-c
	//       infratest/          ← root module
	//         main.tf           ← references ../shared-a, ../shared-b, ../shared-c, remote

	repoRoot := t.TempDir()
	networkDir := filepath.Join(repoRoot, "live", "network")
	rootDir := filepath.Join(networkDir, "infratest")
	require.NoError(t, os.MkdirAll(rootDir, 0o755))

	// Create three local modules one level up from root (live/network/shared-*)
	sharedA := filepath.Join(networkDir, "shared-a")
	sharedB := filepath.Join(networkDir, "shared-b")
	sharedC := filepath.Join(networkDir, "shared-c")
	require.NoError(t, os.MkdirAll(sharedA, 0o755))
	require.NoError(t, os.MkdirAll(sharedB, 0o755))
	require.NoError(t, os.MkdirAll(sharedC, 0o755))
	writeTerraformFile(t, sharedA, "main.tf", `output "a" { value = "alpha" }`)
	writeTerraformFile(t, sharedB, "main.tf", `output "b" { value = "beta" }`)
	writeTerraformFile(t, sharedC, "main.tf", `output "c" { value = "gamma" }`)

	// Root module: references all 3 local ../ modules (no remote — resolver needs network for that)
	writeTerraformFile(t, rootDir, "main.tf", `
module "shared_a" {
  source = "../shared-a"
}

module "shared_b" {
  source = "../shared-b"
}

module "shared_c" {
  source = "../shared-c"
}
`)

	// Use the real resolver to resolve modules from the filesystem.
	// Only resolve local modules — the remote reference will fail without network.
	// The key question: are the local ../ modules in the resolved tree?
	resolver := NewResolver()
	resolver.PackageRoot = repoRoot
	resolver.RootDir = rootDir
	tree, err := resolver.Resolve(context.Background(), rootDir)
	require.NoError(t, err)
	require.NotNil(t, tree)

	t.Logf("Resolved tree: %d modules, %d packages", len(tree.AllModules), len(tree.Packages))
	for _, mod := range tree.AllModules {
		t.Logf("  module: key=%s name=%s local=%v remote=%v install=%s pkgRoot=%s",
			mod.Key, mod.Name, mod.IsLocal, mod.IsRemote, mod.InstallDir, mod.PackageRoot)
	}

	// There should be at least 4 modules: root + 3 local
	require.GreaterOrEqual(t, len(tree.AllModules), 4,
		"expected at least 4 modules (root + 3 local)")

	// Apply sourcetree identity planning (non-OCI flow)
	stripPlan, err := PlanStripping(tree, StripModeModuleDir)
	require.NoError(t, err)
	identityPlan, err := BuildSourcetreeIdentityPlan(tree, stripPlan)
	require.NoError(t, err)
	require.NoError(t, ApplySourcetreeIdentityPlan(tree, identityPlan))

	// Re-plan stripping
	stripPlan, err = PlanStripping(tree, StripModeModuleDir)
	require.NoError(t, err)

	// Bundle to ZIP
	archivePath := filepath.Join(t.TempDir(), "bundle.zip")
	bundler := NewBundler(BundleFormatZIP)
	bundler.StripPlan = stripPlan
	require.NoError(t, bundler.Bundle(tree, archivePath))

	// List all files in the archive
	names := zipFileNames(t, archivePath)
	t.Logf("Archive contents (%d entries):", len(names))
	for _, n := range names {
		t.Logf("  %s", n)
	}

	// All three local modules must be present
	localModules := []string{"shared-a", "shared-b", "shared-c"}
	for _, modName := range localModules {
		found := false
		for _, name := range names {
			if strings.Contains(name, modName+"/") && strings.HasSuffix(name, ".tf") {
				found = true
				break
			}
		}
		assert.True(t, found, "local module %s must be present in archive", modName)
	}

	// Remote package must be present under modules/ (none in this local-only test)
	// Total expected: root main.tf + 3 local main.tf = 4 .tf files
	tfCount := 0
	for _, name := range names {
		if strings.HasSuffix(name, ".tf") {
			tfCount++
		}
	}
	assert.Equal(t, 4, tfCount, "archive must contain exactly 4 .tf files (root + 3 local)")
}

// TestBundler_LocalModulesOutsideRootWithRemote_RealResolver uses a local git file source
// for the remote module so the test can run offline. This exactly reproduces the user's
// scenario: nested root with ../ local references + a remote (git) module.
func TestBundler_LocalModulesOutsideRootWithRemote_RealResolver(t *testing.T) {
	// Create a local git repo for the remote module
	remoteRepo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(remoteRepo, "main.tf"),
		[]byte(`output "remote" { value = "ok" }`), 0o644))
	git := func(args ...string) {
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

	// Layout:
	//   repo/
	//     live/network/
	//       shared-a/           ← ../shared-a
	//       shared-b/           ← ../shared-b
	//       infratest/          ← root module
	//         main.tf           ← references ../shared-a, ../shared-b, remote
	repoRoot := t.TempDir()
	networkDir := filepath.Join(repoRoot, "live", "network")
	rootDir := filepath.Join(networkDir, "infratest")
	require.NoError(t, os.MkdirAll(rootDir, 0o755))

	sharedA := filepath.Join(networkDir, "shared-a")
	sharedB := filepath.Join(networkDir, "shared-b")
	require.NoError(t, os.MkdirAll(sharedA, 0o755))
	require.NoError(t, os.MkdirAll(sharedB, 0o755))
	writeTerraformFile(t, sharedA, "main.tf", `output "a" { value = "alpha" }`)
	writeTerraformFile(t, sharedB, "main.tf", `output "b" { value = "beta" }`)

	writeTerraformFile(t, rootDir, "main.tf", fmt.Sprintf(`
module "shared_a" {
  source = "../shared-a"
}

module "remote_mod" {
  source = %q
}
`, remoteSource))

	resolver := NewResolver()
	resolver.PackageRoot = repoRoot
	resolver.RootDir = rootDir
	tree, err := resolver.Resolve(context.Background(), rootDir)
	require.NoError(t, err)

	t.Logf("Resolved: %d modules, %d packages", len(tree.AllModules), len(tree.Packages))
	for _, mod := range tree.AllModules {
		t.Logf("  %s: local=%v remote=%v", mod.Key, mod.IsLocal, mod.IsRemote)
	}

	require.GreaterOrEqual(t, len(tree.AllModules), 3,
		"expected at least 3 modules (root + 2 local + 1 remote)")
	require.GreaterOrEqual(t, len(tree.Packages), 1,
		"expected at least 1 remote package")

	// Apply sourcetree identity planning
	stripPlan, err := PlanStripping(tree, StripModeModuleDir)
	require.NoError(t, err)
	identityPlan, err := BuildSourcetreeIdentityPlan(tree, stripPlan)
	require.NoError(t, err)
	require.NoError(t, ApplySourcetreeIdentityPlan(tree, identityPlan))

	stripPlan, err = PlanStripping(tree, StripModeModuleDir)
	require.NoError(t, err)

	archivePath := filepath.Join(t.TempDir(), "bundle.zip")
	bundler := NewBundler(BundleFormatZIP)
	bundler.StripPlan = stripPlan
	require.NoError(t, bundler.Bundle(tree, archivePath))

	names := zipFileNames(t, archivePath)
	t.Logf("Archive contents (%d entries):", len(names))
	for _, n := range names {
		t.Logf("  %s", n)
	}

	// All local modules must be present
	for _, modName := range []string{"shared-a"} {
		found := false
		for _, name := range names {
			if strings.Contains(name, modName+"/") && strings.HasSuffix(name, ".tf") {
				found = true
				break
			}
		}
		assert.True(t, found, "local module %s must be present in archive", modName)
	}

	// Remote package must be present under modules/
	foundRemote := false
	for _, name := range names {
		if strings.HasPrefix(name, "modules/") && strings.HasSuffix(name, ".tf") {
			foundRemote = true
			break
		}
	}
	assert.True(t, foundRemote, "remote package must be present under modules/ in archive")
}

// TestBundler_NestedRootWithRemoteModules_AllPackagesInZip reproduces the user-reported bug:
// bundling a root module at a nested path (live/network/infratest/) with remote git-sourced
// modules should include ALL package directories in the ZIP, not just rewrite source references.
func TestBundler_NestedRootWithRemoteModules_AllPackagesInZip(t *testing.T) {
	// Simulate a git repo root containing a nested root module at live/network/infratest/
	// and two remote modules from different packages.
	repoRoot := t.TempDir()
	rootWorkDir := filepath.Join(repoRoot, "live", "network", "infratest")
	require.NoError(t, os.MkdirAll(rootWorkDir, 0o755))

	// Create two distinct remote packages (simulating downloaded git repos)
	pkg1Dir := t.TempDir()
	writeTerraformFile(t, pkg1Dir, "main.tf", `output "pkg1" { value = "one" }`)
	pkg2Dir := t.TempDir()
	writeTerraformFile(t, pkg2Dir, "main.tf", `output "pkg2" { value = "two" }`)

	// Root module references both packages
	writeTerraformFile(t, rootWorkDir, "ssm_outputs.tf", `
module "pkg_one" {
  source = "git::https://github.com/example/pkg-one.git?ref=v1.0.0"
}

module "pkg_two" {
  source = "git::https://github.com/example/pkg-two.git?ref=v2.0.0"
}
`)

	// Build resolved tree: root module, two remote children, two packages
	modPkg1 := &ModuleNode{
		Parent:      nil, // set below
		Key:         "root.pkg_one",
		Name:        "pkg_one",
		Source:      ModuleSource{Raw: "git::https://github.com/example/pkg-one.git?ref=v1.0.0", PackageAddr: "git::https://github.com/example/pkg-one.git", Type: SourceGit},
		InstallDir:  pkg1Dir,
		PackageRoot: pkg1Dir,
		IsRemote:    true,
	}
	modPkg2 := &ModuleNode{
		Parent:      nil, // set below
		Key:         "root.pkg_two",
		Name:        "pkg_two",
		Source:      ModuleSource{Raw: "git::https://github.com/example/pkg-two.git?ref=v2.0.0", PackageAddr: "git::https://github.com/example/pkg-two.git", Type: SourceGit},
		InstallDir:  pkg2Dir,
		PackageRoot: pkg2Dir,
		IsRemote:    true,
	}
	rootModule := &ModuleNode{
		Key:         "root",
		Name:        "root",
		InstallDir:  rootWorkDir,
		PackageRoot: repoRoot, // repo root is the package root (simulating detectRepoRoot)
		IsLocal:     true,
		Children:    []*ModuleNode{modPkg1, modPkg2},
	}
	modPkg1.Parent = rootModule
	modPkg2.Parent = rootModule

	tree := &ResolvedTree{
		Root:       rootModule,
		AllModules: []*ModuleNode{rootModule, modPkg1, modPkg2},
		Packages: map[string]*DownloadedPackage{
			"pkg-one": {PackageAddr: "git::https://github.com/example/pkg-one.git", LocalDir: pkg1Dir},
			"pkg-two": {PackageAddr: "git::https://github.com/example/pkg-two.git", LocalDir: pkg2Dir},
		},
	}

	// Apply sourcetree identity planning (non-OCI flow)
	stripPlan, err := PlanStripping(tree, StripModeModuleDir)
	require.NoError(t, err)
	identityPlan, err := BuildSourcetreeIdentityPlan(tree, stripPlan)
	require.NoError(t, err)
	require.NoError(t, ApplySourcetreeIdentityPlan(tree, identityPlan))

	// Re-plan stripping after identity
	stripPlan, err = PlanStripping(tree, StripModeModuleDir)
	require.NoError(t, err)

	// Bundle to ZIP
	archivePath := filepath.Join(t.TempDir(), "bundle.zip")
	bundler := NewBundler(BundleFormatZIP)
	bundler.StripPlan = stripPlan
	require.NoError(t, bundler.Bundle(tree, archivePath))

	// Extract and verify
	extractDir := t.TempDir()
	extractZip(t, archivePath, extractDir)

	// Verify root module files are at the nested path relative to package root
	rootFile := filepath.Join(extractDir, "live", "network", "infratest", "ssm_outputs.tf")
	require.FileExists(t, rootFile, "root ssm_outputs.tf must be in the archive at its nested path")

	// Read the rewritten source
	content, err := os.ReadFile(rootFile)
	require.NoError(t, err)
	contentStr := string(content)

	// Sources must be rewritten to relative paths pointing to the vendor dir.
	// Since the .tf file is at live/network/infratest/ (3 levels below the package root),
	// the relative path goes up 3 levels then into modules/.
	assert.Contains(t, contentStr, "modules/",
		"remote module sources must be rewritten to point to vendor dir")
	assert.NotContains(t, contentStr, "git::",
		"remote module sources must not contain original git:: references")

	// Find ALL package directories in the archive
	names := zipFileNames(t, archivePath)
	t.Logf("Archive contents: %v", names)

	// Count how many unique package directories exist under modules/
	packageDirs := make(map[string]bool)
	for _, name := range names {
		if after, ok := strings.CutPrefix(name, "modules/"); ok {
			// Extract the package ID (first path component after modules/)
			if part, _, found := strings.Cut(after, "/"); found && part != "" {
				packageDirs[part] = true
			}
		}
	}

	// BOTH packages must be present
	assert.GreaterOrEqual(t, len(packageDirs), 2,
		"archive must contain both package directories under modules/, found: %v", packageDirs)

	// Each package must have at least one .tf file
	for pkgID := range packageDirs {
		found := false
		for _, name := range names {
			if strings.HasPrefix(name, "modules/"+pkgID+"/") && strings.HasSuffix(name, ".tf") {
				found = true
				break
			}
		}
		assert.True(t, found, "package %s must contain .tf files", pkgID)
	}

	// Verify the rewritten source paths actually resolve to existing directories
	// Extract the source path from ssm_outputs.tf
	sourceRe := regexp.MustCompile(`source\s*=\s*"([^"]+)"`)
	matches := sourceRe.FindAllStringSubmatch(contentStr, -1)
	for _, match := range matches {
		sourcePath := match[1]
		// Resolve relative to the .tf file's location
		tfDir := filepath.Join(extractDir, "live", "network", "infratest")
		resolved := filepath.Join(tfDir, sourcePath)
		// Normalize to archive-relative path
		rel, err := filepath.Rel(extractDir, resolved)
		require.NoError(t, err)
		t.Logf("Rewritten source %q resolves to %q in archive", sourcePath, rel)

		// The directory must exist in the archive
		absInExtract := filepath.Join(extractDir, rel)
		assert.DirExists(t, absInExtract,
			"rewritten source %q must resolve to existing directory %q in archive",
			sourcePath, rel)
	}
}

func TestBundler_RejectsVendorDirConflict(t *testing.T) {
	// Root has a local module at modules/local/ AND a remote git dependency.
	// The vendor dir (modules/) would contain both user content (local/) and
	// downloaded packages. Bundling must error rather than silently dropping
	// the user's local module.
	rootDir := t.TempDir()

	writeTerraformFile(t, rootDir, "main.tf", `
module "local" {
  source = "./modules/local"
}

module "remote" {
  source = "git::https://github.com/terraform-aws-modules/terraform-aws-vpc.git?ref=v5.0.0"
  name   = "test-vpc"
}
`)

	localDir := filepath.Join(rootDir, "modules", "local")
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	writeTerraformFile(t, localDir, "main.tf", `# local module`)

	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), rootDir)
	require.NoError(t, err)
	require.NotEmpty(t, tree.Packages, "must have remote packages for the conflict scenario")

	bundler := NewBundler(BundleFormatTarGZ)
	archivePath := filepath.Join(t.TempDir(), "bundle.tar.gz")
	err = bundler.Bundle(tree, archivePath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "conflicts with existing content")
	assert.Contains(t, err.Error(), "local")
	assert.Contains(t, err.Error(), "--vendor-dir")
}

func TestBundler_AllowsVendorDirWhenNoRemotePackages(t *testing.T) {
	// No remote packages => vendor dir skip won't trigger, so no conflict.
	rootDir := t.TempDir()

	writeTerraformFile(t, rootDir, "main.tf", `
module "local" {
  source = "./modules/local"
}
`)

	localDir := filepath.Join(rootDir, "modules", "local")
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	writeTerraformFile(t, localDir, "main.tf", `# local module`)

	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), rootDir)
	require.NoError(t, err)
	require.Empty(t, tree.Packages)

	bundler := NewBundler(BundleFormatTarGZ)
	archivePath := filepath.Join(t.TempDir(), "bundle.tar.gz")
	err = bundler.Bundle(tree, archivePath)
	require.NoError(t, err) // no remote packages, no conflict
}

func TestBundler_AllowsVendorDirWithCustomName(t *testing.T) {
	// Custom --vendor-dir avoids the conflict with user's modules/ directory.
	rootDir := t.TempDir()

	writeTerraformFile(t, rootDir, "main.tf", `
module "local" {
  source = "./modules/local"
}

module "remote" {
  source = "git::https://github.com/terraform-aws-modules/terraform-aws-vpc.git?ref=v5.0.0"
  name   = "test-vpc"
}
`)

	localDir := filepath.Join(rootDir, "modules", "local")
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	writeTerraformFile(t, localDir, "main.tf", `# local module`)

	resolver := NewResolver()
	resolver.VendorDir = testCustomVendorDir
	tree, err := resolver.Resolve(context.Background(), rootDir)
	require.NoError(t, err)
	require.NotEmpty(t, tree.Packages)

	bundler := NewBundler(BundleFormatTarGZ)
	bundler.VendorDir = testCustomVendorDir
	archivePath := filepath.Join(t.TempDir(), "bundle.tar.gz")
	err = bundler.Bundle(tree, archivePath)
	require.NoError(t, err) // custom vendor dir doesn't conflict with modules/
}

// TestBundler_VendorDirConflict_FullIntegrity verifies that when a conflict is
// resolved via --vendor-dir, ALL modules are preserved in the bundle — both user
// local modules under modules/ and remote packages under the custom vendor dir.
// This is a full-scope integrity test: conflict detection, resolution, source
// rewriting, and file presence.
func TestBundler_VendorDirConflict_FullIntegrity(t *testing.T) {
	rootDir := t.TempDir()

	// Root with TWO user local modules under modules/ AND a remote dep
	writeTerraformFile(t, rootDir, "main.tf", `
module "local_a" {
  source = "./modules/local"
  input  = "alpha"
}

module "local_b" {
  source = "./modules/extra"
  input  = module.local_a.out
}

module "remote" {
  source = "git::https://github.com/terraform-aws-modules/terraform-aws-vpc.git?ref=v5.0.0"
  name   = "test-vpc"
}

output "result" {
  value = module.local_b.out
}
`)

	// Create user local modules
	for _, name := range []string{"local", "extra"} {
		dir := filepath.Join(rootDir, "modules", name)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		writeTerraformFile(t, dir, "main.tf", fmt.Sprintf(`
variable "input" { type = string }
output "out" { value = "%s-${var.input}" }
`, name))
	}

	// Phase 1: Default vendor dir must error (conflict)
	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), rootDir)
	require.NoError(t, err)
	require.NotEmpty(t, tree.Packages)

	bundler := NewBundler(BundleFormatTarGZ)
	err = bundler.Bundle(tree, filepath.Join(t.TempDir(), "conflict.tar.gz"))
	require.Error(t, err, "default vendor dir must detect conflict")
	assert.Contains(t, err.Error(), "conflicts with existing content")

	// Phase 2: Re-resolve and bundle with custom vendor dir (fresh root to avoid resolver state)
	rootDir2 := t.TempDir()
	writeTerraformFile(t, rootDir2, "main.tf", `
module "local_a" {
  source = "./modules/local"
  input  = "alpha"
}

module "local_b" {
  source = "./modules/extra"
  input  = module.local_a.out
}

module "remote" {
  source = "git::https://github.com/terraform-aws-modules/terraform-aws-vpc.git?ref=v5.0.0"
  name   = "test-vpc"
}

output "result" {
  value = module.local_b.out
}
`)
	for _, name := range []string{"local", "extra"} {
		dir := filepath.Join(rootDir2, "modules", name)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		writeTerraformFile(t, dir, "main.tf", fmt.Sprintf(`
variable "input" { type = string }
output "out" { value = "%s-${var.input}" }
`, name))
	}

	resolver2 := NewResolver()
	resolver2.VendorDir = testCustomVendorDir
	tree2, err := resolver2.Resolve(context.Background(), rootDir2)
	require.NoError(t, err)
	require.NotEmpty(t, tree2.Packages)

	bundler2 := NewBundler(BundleFormatTarGZ)
	bundler2.VendorDir = testCustomVendorDir
	archivePath := filepath.Join(t.TempDir(), "resolved.tar.gz")
	err = bundler2.Bundle(tree2, archivePath)
	require.NoError(t, err, "custom vendor dir must succeed")

	// Phase 3: Extract and verify complete integrity
	extractDir := t.TempDir()
	extractTarGz(t, archivePath, extractDir)

	// All user local modules must be present
	assert.FileExists(t, filepath.Join(extractDir, "main.tf"), "root main.tf missing")
	assert.FileExists(t, filepath.Join(extractDir, "modules", "local", "main.tf"),
		"user module local/ missing — silently dropped!")
	assert.FileExists(t, filepath.Join(extractDir, "modules", "extra", "main.tf"),
		"user module extra/ missing — silently dropped!")

	// Remote package must be present under custom vendor dir
	vendorDir := filepath.Join(extractDir, testCustomVendorDir)
	assert.DirExists(t, vendorDir, "custom vendor dir missing")
	entries, err := os.ReadDir(vendorDir)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(entries), 1, "vendor dir must contain at least one package")

	// Verify source rewrites in root main.tf
	mainContent, err := os.ReadFile(filepath.Join(extractDir, "main.tf"))
	require.NoError(t, err)
	mainStr := string(mainContent)

	// User local module sources must be UNCHANGED (relative paths)
	assert.Contains(t, mainStr, `source = "./modules/local"`,
		"user local module source must not be rewritten")
	assert.Contains(t, mainStr, `source = "./modules/extra"`,
		"user local module source must not be rewritten")

	// Remote module source must be rewritten to custom vendor dir
	assert.Contains(t, mainStr, `source = "./_vendor/`,
		"remote module source must point to custom vendor dir")

	// Verify no dangling sourcetree references (old naming)
	assert.NotContains(t, mainStr, "sourcetree",
		"bundle must not contain old sourcetree naming")

	// Verify the user module files have correct content (not corrupted by rewrite)
	localContent, err := os.ReadFile(filepath.Join(extractDir, "modules", "local", "main.tf"))
	require.NoError(t, err)
	assert.Contains(t, string(localContent), "local-${var.input}",
		"user module content must be preserved")

	extraContent, err := os.ReadFile(filepath.Join(extractDir, "modules", "extra", "main.tf"))
	require.NoError(t, err)
	assert.Contains(t, string(extraContent), "extra-${var.input}",
		"user module content must be preserved")
}

// TestBundler_NoConflict_AllModulesPresent verifies that when the user's local
// modules are NOT in the vendor directory, the default vendor dir works correctly
// and all modules (local + remote) are present in the bundle.
func TestBundler_NoConflict_AllModulesPresent(t *testing.T) {
	rootDir := t.TempDir()

	// User modules in local_modules/ — does NOT conflict with modules/ vendor dir
	writeTerraformFile(t, rootDir, "main.tf", `
module "user_local" {
  source = "./local_modules/app"
}

module "remote" {
  source = "git::https://github.com/terraform-aws-modules/terraform-aws-vpc.git?ref=v5.0.0"
  name   = "test-vpc"
}
`)

	localDir := filepath.Join(rootDir, "local_modules", "app")
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	writeTerraformFile(t, localDir, "main.tf", `output "app" { value = "user-local" }`)

	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), rootDir)
	require.NoError(t, err)
	require.NotEmpty(t, tree.Packages, "must have remote packages")

	bundler := NewBundler(BundleFormatTarGZ)
	archivePath := filepath.Join(t.TempDir(), "bundle.tar.gz")
	err = bundler.Bundle(tree, archivePath)
	require.NoError(t, err, "no conflict — default vendor dir must work")

	extractDir := t.TempDir()
	extractTarGz(t, archivePath, extractDir)

	// User local module must be present
	assert.FileExists(t, filepath.Join(extractDir, "local_modules", "app", "main.tf"),
		"user local module missing")

	// Remote packages must be present in vendor dir
	assert.DirExists(t, filepath.Join(extractDir, "modules"),
		"vendor dir (modules/) missing")

	entries, err := os.ReadDir(filepath.Join(extractDir, "modules"))
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(entries), 1, "vendor dir must contain packages")

	// Source rewrites must be correct
	mainContent, err := os.ReadFile(filepath.Join(extractDir, "main.tf"))
	require.NoError(t, err)
	mainStr := string(mainContent)
	assert.Contains(t, mainStr, `source = "./local_modules/app"`,
		"user module source must be preserved")
	assert.Contains(t, mainStr, `source = "./modules/`,
		"remote module source must point to vendor dir")
}

// TestBundler_SelfReferencingModuleDoesNotDropPackages manually constructs a tree
// matching the customer report: a module with key X has itself listed as a child.
// This tests that bundling includes ALL packages even with a self-referencing module.
func TestBundler_SelfReferencingModuleDoesNotDropPackages(t *testing.T) {
	// Create directories for 5 packages
	pkgDirs := make([]string, 5)
	for i := range 5 {
		d := t.TempDir()
		writeTerraformFile(t, d, "main.tf", fmt.Sprintf(`output "pkg" { value = "pkg-%d" }`, i))
		pkgDirs[i] = d
	}

	// Create root module dir
	rootWorkDir := t.TempDir()
	writeTerraformFile(t, rootWorkDir, "main.tf", `# root`)

	// Root module
	rootModule := &ModuleNode{
		Key:         "",
		Name:        "root",
		InstallDir:  rootWorkDir,
		PackageRoot: rootWorkDir,
		IsLocal:     true,
	}

	// account_config (local, child of root, at its own directory)
	accConfigDir := filepath.Join(rootWorkDir, "account_config")
	require.NoError(t, os.MkdirAll(accConfigDir, 0o755))
	modAccountConfig := &ModuleNode{
		Key:        "account_config",
		Name:       "account_config",
		InstallDir: accConfigDir,
		Parent:     rootModule,
		IsLocal:    true,
	}

	// account_config.accountindex (local, child of account_config, at its own dir)
	accIndexDir := filepath.Join(rootWorkDir, "account_index")
	require.NoError(t, os.MkdirAll(accIndexDir, 0o755))
	modAccountIndex := &ModuleNode{
		Key:        "account_config.accountindex",
		Name:       "accountindex",
		InstallDir: accIndexDir,
		Parent:     modAccountConfig,
		IsLocal:    true,
	}

	// account_config.accountindex.accountindex (remote, self-referencing)
	modSelfRef := &ModuleNode{
		Key:         "account_config.accountindex.accountindex",
		Name:        "accountindex",
		Source:      ModuleSource{PackageAddr: "git::https://example.com/base.git?ref=v0.0.6", Type: SourceGit},
		InstallDir:  pkgDirs[0],
		PackageRoot: pkgDirs[0],
		Parent:      modAccountIndex,
		IsRemote:    true,
	}
	// THE BUG: module has itself as a child
	modSelfRef.Children = []*ModuleNode{modSelfRef}

	modAccountIndex.Children = []*ModuleNode{modSelfRef}
	modAccountConfig.Children = []*ModuleNode{modAccountIndex}

	// Remote modules referencing other packages
	modOutputs := &ModuleNode{
		Key:         "outputs",
		Name:        "outputs",
		Source:      ModuleSource{PackageAddr: "git::https://example.com/base.git?ref=v0.0.6", Type: SourceGit},
		InstallDir:  pkgDirs[1],
		PackageRoot: pkgDirs[1],
		Parent:      rootModule,
		IsRemote:    true,
	}
	modVpc := &ModuleNode{
		Key:         "vpc",
		Name:        "vpc",
		Source:      ModuleSource{PackageAddr: "git::https://example.com/base.git?ref=v0.9.0", Type: SourceGit},
		InstallDir:  pkgDirs[2],
		PackageRoot: pkgDirs[2],
		Parent:      rootModule,
		IsRemote:    true,
	}
	modNacl := &ModuleNode{
		Key:         "nacl_private",
		Name:        "nacl_private",
		Source:      ModuleSource{PackageAddr: "git::https://example.com/base.git?ref=v0.0.33", Type: SourceGit},
		InstallDir:  pkgDirs[3],
		PackageRoot: pkgDirs[3],
		Parent:      rootModule,
		IsRemote:    true,
	}
	modFlowlog := &ModuleNode{
		Key:         "flowlogs",
		Name:        "flowlogs",
		Source:      ModuleSource{PackageAddr: "git::https://example.com/flowlog.git?ref=v1.1.0", Type: SourceGit},
		InstallDir:  pkgDirs[4],
		PackageRoot: pkgDirs[4],
		Parent:      rootModule,
		IsRemote:    true,
	}
	rootModule.Children = []*ModuleNode{modAccountConfig, modOutputs, modVpc, modNacl, modFlowlog}

	tree := &ResolvedTree{
		Root: rootModule,
		AllModules: []*ModuleNode{
			rootModule, modAccountConfig, modAccountIndex, modSelfRef,
			modOutputs, modVpc, modNacl, modFlowlog,
		},
		Packages: map[string]*DownloadedPackage{
			"pkg-0": {PackageAddr: "git::https://example.com/base.git?ref=v0.0.6", LocalDir: pkgDirs[0]},
			"pkg-1": {PackageAddr: "git::https://example.com/base.git?ref=v0.0.6", LocalDir: pkgDirs[1]},
			"pkg-2": {PackageAddr: "git::https://example.com/base.git?ref=v0.9.0", LocalDir: pkgDirs[2]},
			"pkg-3": {PackageAddr: "git::https://example.com/base.git?ref=v0.0.33", LocalDir: pkgDirs[3]},
			"pkg-4": {PackageAddr: "git::https://example.com/flowlog.git?ref=v1.1.0", LocalDir: pkgDirs[4]},
		},
	}

	// Verify the self-referencing module
	bugMod := tree.Find("account_config.accountindex.accountindex")
	require.NotNil(t, bugMod)
	require.Len(t, bugMod.Children, 1, "self-referencing module should have 1 child")
	assert.Same(t, bugMod, bugMod.Children[0], "module should reference itself as child")

	// Apply sourcetree identity planning (same as bundle command)
	stripPlan, err := PlanStripping(tree, StripModeModuleDir)
	require.NoError(t, err)

	t.Logf("Before identity: %d packages in tree, %d packages in strip plan", len(tree.Packages), len(stripPlan.Packages))

	identityPlan, err := BuildSourcetreeIdentityPlan(tree, stripPlan)
	require.NoError(t, err)

	t.Logf("Identity plan: %d packages, %d by final ID", len(identityPlan.Packages), len(identityPlan.ByFinalID))

	require.NoError(t, ApplySourcetreeIdentityPlan(tree, identityPlan))

	// After identity: verify ALL final dirs exist on disk (sanity check)
	for id, identity := range identityPlan.ByFinalID {
		_, statErr := os.Stat(identity.FinalLocalDir)
		t.Logf("  final dir for %s: %s (exists=%v)", id, identity.FinalLocalDir, statErr == nil)
	}

	// Verify old dirs were removed
	for _, pkgDir := range pkgDirs {
		_, statErr := os.Stat(pkgDir)
		t.Logf("  old dir %s (exists=%v)", pkgDir, statErr == nil)
	}

	// Re-plan stripping
	stripPlan, err = PlanStripping(tree, StripModeModuleDir)
	require.NoError(t, err)

	t.Logf("After identity: %d packages in tree", len(tree.Packages))
	for id, pkg := range tree.Packages {
		_, statErr := os.Stat(pkg.LocalDir)
		t.Logf("  %s: addr=%s dir=%s (exists=%v)", id, pkg.PackageAddr, pkg.LocalDir, statErr == nil)
	}

	// Bundle to ZIP
	archivePath := filepath.Join(t.TempDir(), "bundle.zip")
	bundler := NewBundler(BundleFormatZIP)
	bundler.StripPlan = stripPlan
	err = bundler.Bundle(tree, archivePath)
	require.NoError(t, err, "bundling should complete without errors")

	// Verify ALL packages are in the archive
	names := zipFileNames(t, archivePath)
	packageDirs := make(map[string]bool)
	for _, name := range names {
		if after, ok := strings.CutPrefix(name, "modules/"); ok {
			if part, _, found := strings.Cut(after, "/"); found && part != "" {
				packageDirs[part] = true
			}
		}
	}
	t.Logf("Package dirs in archive: %v (total %d)", packageDirs, len(packageDirs))

	assert.GreaterOrEqual(t, len(packageDirs), 5,
		"archive must contain all 5 package directories, found %d: %v",
		len(packageDirs), packageDirs)
}

// TestResolver_SamePkgDifferentSubdirectories_LocalChainWithSameChildName reproduces
// the EXACT customer scenario. The terrafom-modules-base repo contains a modules/
// subdirectory (for helper modules). The bundler's addDirectoryToZip skips ANY
// directory named "modules" when packages are present, thinking it's the vendor dir.
// This drops all package content from packages that internally use modules/ directories.
func TestResolver_SamePkgDifferentSubdirectories_LocalChainWithSameChildName(t *testing.T) {
	// Create the "remote" package (simulating terraform-modules-base at v0.0.6)
	// It contains a modules/ directory with helper sub-modules.
	remoteRepo := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(remoteRepo, "modules", "helper", "ssm_output"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(remoteRepo, "modules", "helper", "account_index"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(remoteRepo, "modules", "helper", "mandatory_tags"), 0o755))
	writeTerraformFile(t, filepath.Join(remoteRepo, "modules", "helper", "ssm_output"), "main.tf",
		`# ssm_output helper (leaf)`)
	writeTerraformFile(t, filepath.Join(remoteRepo, "modules", "helper", "account_index"), "main.tf",
		`# account_index helper (leaf)`)
	writeTerraformFile(t, filepath.Join(remoteRepo, "modules", "helper", "mandatory_tags"), "main.tf",
		`# mandatory_tags helper (leaf)`)

	git := func(args ...string) {
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
	git("tag", "v0.0.6")

	remoteGitURL := "git::" + (&url.URL{Scheme: "file", Path: remoteRepo}).String()

	// Second remote package (simulating a different repo/ref, like v0.0.33)
	remoteRepo2 := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(remoteRepo2, "modules", "networking", "nacl", "default"), 0o755))
	writeTerraformFile(t, filepath.Join(remoteRepo2, "modules", "networking", "nacl", "default"), "main.tf",
		`# nacl default (leaf)`)
	git2 := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = remoteRepo2
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, string(out))
	}
	git2("init")
	git2("config", "user.email", "test@example.invalid")
	git2("config", "user.name", "TofuPress Test")
	git2("add", ".")
	git2("commit", "-m", "initial")
	git2("tag", "v0.0.33")

	remoteGitURL2 := "git::" + (&url.URL{Scheme: "file", Path: remoteRepo2}).String()

	// Build the monorepo structure matching the customer report
	repoRoot := t.TempDir()
	modulesDir := filepath.Join(repoRoot, "modules")
	networkDir := filepath.Join(repoRoot, "live", "network")
	rootDir := filepath.Join(networkDir, "infratest")
	require.NoError(t, os.MkdirAll(rootDir, 0o755))

	// Local module: account_config
	accConfigDir := filepath.Join(modulesDir, "account_config")
	require.NoError(t, os.MkdirAll(accConfigDir, 0o755))
	writeTerraformFile(t, accConfigDir, "main.tf", `
module "accountindex" {
  source = "../account_index"
}
`)

	// Local module: account_index (has child named "accountindex" referencing remote)
	accIndexDir := filepath.Join(modulesDir, "account_index")
	require.NoError(t, os.MkdirAll(accIndexDir, 0o755))
	writeTerraformFile(t, accIndexDir, "main.tf", fmt.Sprintf(`
module "accountindex" {
  source = "%s//modules/helper/account_index?ref=v0.0.6"
}
`, remoteGitURL))

	// Root module: references account_config and outputs (same pkg, diff subdir)
	// Also references nacl_private (diff pkg)
	writeTerraformFile(t, rootDir, "main.tf", fmt.Sprintf(`
module "account_config" {
  source = "../../../modules/account_config"
}

module "outputs" {
  source = "%s//modules/helper/ssm_output?ref=v0.0.6"
}

module "nacl_private" {
  source = "%s//modules/networking/nacl/default?ref=v0.0.33"
}
`, remoteGitURL, remoteGitURL2))

	// Resolve
	resolver := NewResolver()
	resolver.PackageRoot = repoRoot
	resolver.RootDir = rootDir
	tree, err := resolver.Resolve(context.Background(), rootDir)
	require.NoError(t, err)
	require.NotNil(t, tree)

	// Verify: no module has itself as a child (self-referencing bug)
	for _, mod := range tree.AllModules {
		for _, child := range mod.Children {
			assert.NotEqual(t, mod.Key, child.Key,
				"module %s must not reference itself as a child", mod.Key)
		}
	}
	assert.GreaterOrEqual(t, len(tree.AllModules), 5,
		"expected at least 5 modules")
	assert.GreaterOrEqual(t, len(tree.Packages), 2,
		"expected at least 2 remote packages")

	// Verify the critical module doesn't self-reference
	bugMod := tree.Find("account_config.accountindex.accountindex")
	require.NotNil(t, bugMod)
	assert.Empty(t, bugMod.Children,
		"account_config.accountindex.accountindex must not self-reference")

	// Run through the full bundle pipeline
	stripPlan, err := PlanStripping(tree, StripModeModuleDir)
	require.NoError(t, err)

	identityPlan, err := BuildSourcetreeIdentityPlan(tree, stripPlan)
	require.NoError(t, err)

	require.NoError(t, ApplySourcetreeIdentityPlan(tree, identityPlan))

	// Verify all package dirs exist after identity
	for _, pkg := range tree.Packages {
		_, statErr := os.Stat(pkg.LocalDir)
		require.NoError(t, statErr, "package dir must exist after identity: %s", pkg.LocalDir)
	}

	// Rebuild strip plan and bundle
	stripPlan, err = PlanStripping(tree, StripModeModuleDir)
	require.NoError(t, err)

	archivePath := filepath.Join(t.TempDir(), "bundle.zip")
	bundler := NewBundler(BundleFormatZIP)
	bundler.StripPlan = stripPlan
	require.NoError(t, bundler.Bundle(tree, archivePath))

	// Verify ALL packages are in the archive (no packages dropped due to vendor-dir skip)
	names := zipFileNames(t, archivePath)
	packageDirs := make(map[string]bool)
	for _, name := range names {
		if after, ok := strings.CutPrefix(name, "modules/"); ok {
			if part, _, found := strings.Cut(after, "/"); found && part != "" {
				packageDirs[part] = true
			}
		}
	}

	assert.Len(t, packageDirs, len(identityPlan.ByFinalID),
		"archive must contain all unique packages from identity plan (bug: vendor dir skip in addDirectoryToZip)")

	// Verify metadata matches
	metadata, err := BuildArtifactMetadata(tree, &MetadataRequest{
		Command:    "bundle",
		RootSource: rootDir,
		OutputPath: archivePath,
		StripPlan:  stripPlan,
	})
	require.NoError(t, err)
	assert.Equal(t, len(identityPlan.ByFinalID), metadata.Stats.UniquePackages)
}

// TestBundler_VendorDirSkippingInternalModulesDir reproduces the core bug:
// When a downloaded package contains its own modules/ directory (like
// terraform-modules-base which has modules/helper/...), the bundler's
// addDirectoryToZip incorrectly skips it, treating it as the vendor dir.
//
// This causes packages with internal modules/ structure to have all their
// content dropped from the bundle archive.
func TestBundler_VendorDirSkippingInternalModulesDir(t *testing.T) {
	// Create a minimal tree where a remote package has a modules/ subdir
	rootWorkDir := t.TempDir()

	// Root module
	rootModule := &ModuleNode{
		Key:         "",
		Name:        "root",
		InstallDir:  rootWorkDir,
		PackageRoot: rootWorkDir,
		IsLocal:     true,
	}

	// Create root tf file
	err := os.WriteFile(filepath.Join(rootWorkDir, "main.tf"), []byte(`# root`), 0o644)
	require.NoError(t, err)

	// Create a package with its own modules/ directory
	pkgDir := filepath.Join(rootWorkDir, "modules", "pkg-abc123")
	require.NoError(t, os.MkdirAll(filepath.Join(pkgDir, "modules", "helper"), 0o755))
	err = os.WriteFile(filepath.Join(pkgDir, "modules", "helper", "main.tf"),
		[]byte(`resource "null_resource" "helper" {}`), 0o644)
	require.NoError(t, err)

	// Child module (remote, references the package)
	childModule := &ModuleNode{
		Key:         "child",
		Name:        "child",
		InstallDir:  filepath.Join(pkgDir, "modules", "helper"),
		PackageRoot: pkgDir,
		Parent:      rootModule,
		IsRemote:    true,
		IsLocal:     false,
	}
	rootModule.Children = []*ModuleNode{childModule}

	tree := &ResolvedTree{
		Root:       rootModule,
		AllModules: []*ModuleNode{rootModule, childModule},
		Packages: map[string]*DownloadedPackage{
			"pkg-abc123": {
				PackageAddr: "git::https://example.com/base.git?ref=v1.0",
				LocalDir:    pkgDir,
			},
		},
		VendorDir: "modules",
	}

	// Apply identity planning (no-op since LocalDir IS the final dir)
	stripPlan, err := PlanStripping(tree, StripModeModuleDir)
	require.NoError(t, err)

	identityPlan, err := BuildSourcetreeIdentityPlan(tree, stripPlan)
	require.NoError(t, err)

	// In this case old==final, so materialization is a no-op
	t.Logf("Identity: %d by final ID", len(identityPlan.ByFinalID))

	// Build a fresh strip plan (after identity)
	stripPlan, err = PlanStripping(tree, StripModeModuleDir)
	require.NoError(t, err)

	// Bundle
	archivePath := filepath.Join(t.TempDir(), "bundle.zip")
	bundler := NewBundler(BundleFormatZIP)
	bundler.StripPlan = stripPlan
	bundler.VendorDir = "modules"
	require.NoError(t, bundler.Bundle(tree, archivePath))

	// Verify all entries in the archive
	names := zipFileNames(t, archivePath)
	t.Logf("Archive entries: %d", len(names))
	for _, n := range names {
		t.Logf("  %s", n)
	}

	// The helper module's main.tf MUST be in the archive
	// It should be at modules/pkg-abc123/modules/helper/main.tf
	expectedEntry := "modules/pkg-abc123/modules/helper/main.tf"
	found := false
	for _, n := range names {
		if n == expectedEntry {
			found = true
		}
	}
	assert.True(t, found,
		"package content with internal modules/ directory must be in archive at %s",
		expectedEntry)
}

func TestBundler_TarGzFromDir_Basic(t *testing.T) {
	srcDir := t.TempDir()
	writeTerraformFile(t, srcDir, "main.tf", `output "name" { value = "root" }`)
	writeTerraformFile(t, srcDir, "variables.tf", `variable "env" { type = string }`)
	require.NoError(t, os.MkdirAll(filepath.Join(srcDir, "modules", "pkg-abc123"), 0o755))
	writeTerraformFile(t, filepath.Join(srcDir, "modules", "pkg-abc123"), "main.tf", `output "pkg" { value = "yes" }`)

	archivePath := filepath.Join(t.TempDir(), "bundle.tar.gz")
	bundler := NewBundler(BundleFormatTarGZ)
	err := bundler.bundleTarGzFromDir(srcDir, archivePath)
	require.NoError(t, err)

	names := tarGzFileNames(t, archivePath)
	assert.Contains(t, names, "main.tf")
	assert.Contains(t, names, "variables.tf")
	assert.Contains(t, names, "modules/pkg-abc123/main.tf")
	// modules/ directory must be included (no vendor-dir skip)
	t.Logf("Archive: %v", names)
}

func TestBundler_TarGzFromDir_SkipsTerraformAndGitDirs(t *testing.T) {
	srcDir := t.TempDir()
	writeTerraformFile(t, srcDir, "main.tf", `# root`)
	require.NoError(t, os.MkdirAll(filepath.Join(srcDir, ".terraform", "modules"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, ".terraform", "terraform.tfstate"), []byte("state"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(srcDir, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, ".git", "HEAD"), []byte("ref"), 0o644))

	archivePath := filepath.Join(t.TempDir(), "bundle.tar.gz")
	bundler := NewBundler(BundleFormatTarGZ)
	err := bundler.bundleTarGzFromDir(srcDir, archivePath)
	require.NoError(t, err)

	names := tarGzFileNames(t, archivePath)
	assert.Contains(t, names, "main.tf")
	for _, name := range names {
		assert.NotContains(t, name, ".terraform/")
		assert.NotContains(t, name, ".git/")
	}
}

func TestBundler_TarGzFromDir_EmbedsMetadata(t *testing.T) {
	srcDir := t.TempDir()
	writeTerraformFile(t, srcDir, "main.tf", `# root`)
	meta := &ArtifactMetadata{SchemaVersion: MetadataSchemaVersion, CreatedAt: "2026-06-24T00:00:00Z"}
	require.NoError(t, WriteMetadataFile(filepath.Join(srcDir, MetadataFileName), meta))

	archivePath := filepath.Join(t.TempDir(), "bundle.tar.gz")
	bundler := NewBundler(BundleFormatTarGZ)
	err := bundler.bundleTarGzFromDir(srcDir, archivePath)
	require.NoError(t, err)

	extractDir := t.TempDir()
	extractTarGz(t, archivePath, extractDir)
	data, err := os.ReadFile(filepath.Join(extractDir, MetadataFileName))
	require.NoError(t, err)
	assert.Contains(t, string(data), `"schema_version": "1"`)
}
