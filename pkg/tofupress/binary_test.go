//go:build integration

package tofupress

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildBinary compiles the tofupress binary and returns its path.
// This is a test helper for binary integration tests.
func buildBinary(t *testing.T) string {
	t.Helper()

	// Create temp directory for binary
	tmpDir := t.TempDir()
	binaryName := "tofupress"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	binaryPath := filepath.Join(tmpDir, binaryName)

	// Get the project root directory (3 levels up from this test file)
	_, filename, _, _ := runtime.Caller(0)
	projectRoot := filepath.Dir(filepath.Dir(filepath.Dir(filename)))

	// Build the binary from project root
	cmd := exec.Command("go", "build", "-o", binaryPath, "./cmd/tofupress") //nolint:gosec // G204: subprocess is intentional for building test binary
	cmd.Dir = projectRoot
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "failed to build binary: %s", string(output))

	// Verify binary exists
	_, err = os.Stat(binaryPath)
	require.NoError(t, err, "binary should exist at %s", binaryPath)

	return binaryPath
}

// createSimpleFixture creates a minimal Terraform module with one local dependency.
// Returns the path to the fixture directory.
func createSimpleFixture(t *testing.T) string {
	t.Helper()

	// Create temp directory for fixture
	fixtureDir := t.TempDir()

	// Write root module
	writeTerraformFile(t, fixtureDir, "main.tf", `
module "child" {
  source = "./child"
}
`)

	// Write child module
	childDir := filepath.Join(fixtureDir, "child")
	require.NoError(t, os.MkdirAll(childDir, 0o755)) //nolint:gosec // G301: 0755 is fine for test directories
	writeTerraformFile(t, childDir, "main.tf", `# child module`)

	return fixtureDir
}

// validateZipFile opens a zip archive and verifies it has at least one entry.
func validateZipFile(t *testing.T, path string) error {
	t.Helper()

	f, err := zip.OpenReader(path)
	if err != nil {
		return fmt.Errorf("failed to open zip: %w", err)
	}
	defer f.Close() //nolint:errcheck // best effort

	if len(f.File) == 0 {
		return fmt.Errorf("zip archive is empty")
	}

	return nil
}

// validateTarGzFile opens a tar.gz archive and verifies it has at least one entry.
func validateTarGzFile(t *testing.T, path string) error {
	t.Helper()

	f, err := os.Open(path) //nolint:gosec // G304: path comes from test
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer f.Close() //nolint:errcheck // best effort

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gz.Close() //nolint:errcheck // best effort

	tr := tar.NewReader(gz)

	// Try to read at least one entry
	_, err = tr.Next()
	if errors.Is(err, io.EOF) {
		return fmt.Errorf("tar.gz archive is empty")
	}
	if err != nil {
		return fmt.Errorf("failed to read tar entry: %w", err)
	}

	return nil
}

// TestBinary_HelpShowsCommands verifies that the CLI binary shows resolve and bundle commands in help.
func TestBinary_HelpShowsCommands(t *testing.T) {
	binary := buildBinary(t)

	// Run the binary with --help flag
	cmd := exec.Command(binary, "--help") //nolint:gosec // G204: subprocess is intentional for testing binary
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "binary --help should succeed: %s", string(output))

	outputStr := string(output)

	// Verify help output contains the expected commands
	assert.Contains(t, outputStr, "Available Commands", "help output should have an Available Commands section")
	assert.Contains(t, outputStr, "  resolve", "help output should list 'resolve' as a command")
	assert.Contains(t, outputStr, "  bundle", "help output should list 'bundle' as a command")
}

// TestBinary_ResolveLocalDir verifies that the resolve command works on a local directory.
func TestBinary_ResolveLocalDir(t *testing.T) {
	binary := buildBinary(t)
	fixture := createSimpleFixture(t)

	// Run the binary with resolve command
	cmd := exec.Command(binary, "resolve", fixture) //nolint:gosec // G204: subprocess is intentional for testing binary
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "binary resolve should succeed: %s", string(output))

	outputStr := string(output)

	// Verify output contains expected sections
	assert.Contains(t, outputStr, "Module tree:", "output should contain 'Module tree:' header")
	assert.Contains(t, outputStr, "root", "output should contain 'root' module")
	assert.Contains(t, outputStr, "child", "output should contain 'child' module")
	assert.Contains(t, outputStr, "Total modules:", "output should contain module count")
}

// TestBinary_BundleCreatesArchive verifies that the bundle command creates a valid zip archive.
func TestBinary_BundleCreatesArchive(t *testing.T) {
	binary := buildBinary(t)
	fixture := createSimpleFixture(t)
	outputPath := filepath.Join(t.TempDir(), "bundle.zip")

	// Run the binary with bundle command
	cmd := exec.Command(binary, "bundle", fixture, outputPath) //nolint:gosec // G204: subprocess is intentional for testing binary
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "binary bundle should succeed: %s", string(output))

	// Verify archive exists
	info, err := os.Stat(outputPath)
	require.NoError(t, err, "bundle file should exist")
	assert.Greater(t, info.Size(), int64(0), "bundle file should not be empty")

	// Verify it's a valid zip with entries
	err = validateZipFile(t, outputPath)
	require.NoError(t, err, "bundle should be a valid zip archive")
}

// TestBinary_BundlePreservesSource verifies that bundling does NOT modify source files.
// This test was added in RED phase and now passes after implementing temp directory isolation.
func TestBinary_BundlePreservesSource(t *testing.T) {
	binary := buildBinary(t)
	fixture := createSimpleFixture(t)
	outputPath := filepath.Join(t.TempDir(), "bundle.zip")

	// Record original state of all source files
	mainTfPath := filepath.Join(fixture, "main.tf")
	childTfPath := filepath.Join(fixture, "child", "main.tf")

	// Get original checksums
	originalMainHash := hashFile(t, mainTfPath)
	originalChildHash := hashFile(t, childTfPath)

	// Get original modification times
	originalMainInfo, err := os.Stat(mainTfPath)
	require.NoError(t, err)
	originalMainModTime := originalMainInfo.ModTime()

	originalChildInfo, err := os.Stat(childTfPath)
	require.NoError(t, err)
	originalChildModTime := originalChildInfo.ModTime()

	// Run bundle command
	cmd := exec.Command(binary, "bundle", fixture, outputPath) //nolint:gosec // G204: subprocess is intentional for testing binary
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "bundle should succeed: %s", string(output))

	// Verify bundle was created
	_, err = os.Stat(outputPath)
	require.NoError(t, err, "bundle file should exist")

	// CRITICAL: Verify source files were NOT modified
	afterMainHash := hashFile(t, mainTfPath)
	afterChildHash := hashFile(t, childTfPath)

	assert.Equal(t, originalMainHash, afterMainHash,
		"main.tf checksum changed - source file was modified!")
	assert.Equal(t, originalChildHash, afterChildHash,
		"child/main.tf checksum changed - source file was modified!")

	// Verify modification times did NOT change
	afterMainInfo, err := os.Stat(mainTfPath)
	require.NoError(t, err)
	assert.Equal(t, originalMainModTime, afterMainInfo.ModTime(),
		"main.tf modification time changed - source file was touched!")

	afterChildInfo, err := os.Stat(childTfPath)
	require.NoError(t, err)
	assert.Equal(t, originalChildModTime, afterChildInfo.ModTime(),
		"child/main.tf modification time changed - source file was touched!")

	// Verify no sourcetree/ directory was created in source directory
	sourcetreePath := filepath.Join(fixture, "sourcetree")
	_, err = os.Stat(sourcetreePath)
	assert.True(t, os.IsNotExist(err),
		"sourcetree/ directory should NOT exist in source directory - bundling polluted the source!")
}

// hashFile returns the SHA-256 hash of a file's contents.
func hashFile(t *testing.T, path string) [32]byte {
	t.Helper()
	content, err := os.ReadFile(path) //nolint:gosec // G304: path comes from test fixture
	require.NoError(t, err)
	return sha256.Sum256(content)
}

// TestBinary_BundleAutoDetectsFormat verifies that bundle auto-detects format from file extension.
func TestBinary_BundleAutoDetectsFormat(t *testing.T) {
	binary := buildBinary(t)
	fixture := createSimpleFixture(t)
	outputPath := filepath.Join(t.TempDir(), "bundle.tar.gz")

	// Run the binary with bundle command (no --format flag)
	cmd := exec.Command(binary, "bundle", fixture, outputPath) //nolint:gosec // G204: subprocess is intentional for testing binary
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "binary bundle should succeed: %s", string(output))

	// Verify archive exists
	info, err := os.Stat(outputPath)
	require.NoError(t, err, "bundle file should exist")
	assert.Greater(t, info.Size(), int64(0), "bundle file should not be empty")

	// Verify it's a valid tar.gz (not zip)
	err = validateTarGzFile(t, outputPath)
	require.NoError(t, err, "bundle should be a valid tar.gz archive (auto-detected from .tar.gz extension)")
}

// TestBinary_BundleRemoteGitSource verifies that bundle accepts a remote git source
// directly as the root input (not just local directories).
// This test was added in RED phase and now passes after implementing remote source support.
func TestBinary_BundleRemoteGitSource(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test with network access")
	}

	binary := buildBinary(t)
	outputPath := filepath.Join(t.TempDir(), "bundle.zip")

	// Bundle a remote git module directly — implementation accepts both local and remote sources
	cmd := exec.Command(binary, "bundle", //nolint:gosec // G204: subprocess is intentional for testing binary
		"git::https://github.com/terraform-aws-modules/terraform-aws-vpc.git?ref=master",
		outputPath)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "bundle remote git failed: %s", string(out))

	// Verify bundle was created
	_, err = os.Stat(outputPath)
	require.NoError(t, err, "bundle file should exist")

	// Verify it's a valid zip
	err = validateZipFile(t, outputPath)
	require.NoError(t, err, "bundle should be a valid zip archive")
}

// TestBinary_BundleNoDuplicates verifies that bundling a project with remote modules
// does not create duplicate entries in the archive.
// This test was added in RED phase to catch the duplicate archive entries bug.
func TestBinary_BundleNoDuplicates(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test with network access")
	}

	binary := buildBinary(t)

	// Create a local fixture with a remote module reference
	fixtureDir := t.TempDir()
	writeTerraformFile(t, fixtureDir, "main.tf", `
module "vpc" {
  source = "git::https://github.com/terraform-aws-modules/terraform-aws-vpc.git?ref=master"
}
`)

	outputPath := filepath.Join(t.TempDir(), "bundle.zip")

	// Bundle the local fixture (which will download the remote module)
	cmd := exec.Command(binary, "bundle", fixtureDir, outputPath) //nolint:gosec // G204: subprocess is intentional for testing binary
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "bundle should succeed: %s", string(out))

	// Verify bundle was created
	_, err = os.Stat(outputPath)
	require.NoError(t, err, "bundle file should exist")

	// Open the zip and check for duplicate entries
	f, err := zip.OpenReader(outputPath)
	require.NoError(t, err, "failed to open zip archive")
	defer f.Close() //nolint:errcheck // best effort

	// Count occurrences of each file path
	entryCounts := make(map[string]int)
	for _, file := range f.File {
		entryCounts[file.Name]++
	}

	// Check that no entry appears more than once
	for path, count := range entryCounts {
		assert.Equal(t, 1, count, "archive entry %s appears %d times (should be 1) - duplicate detected!", path, count)
	}

	// Also verify we have at least some entries (sanity check)
	assert.Greater(t, len(entryCounts), 0, "archive should contain at least one entry")
}

// TestBinary_ResolveRemoteGitJSON verifies that resolve with --json works on a remote git source.
// This test was added in RED phase and now passes after implementing remote source support.
func TestBinary_ResolveRemoteGitJSON(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test with network access")
	}

	binary := buildBinary(t)

	cmd := exec.Command(binary, "resolve", "--json", //nolint:gosec // G204: subprocess is intentional for testing binary
		"git::https://github.com/terraform-aws-modules/terraform-aws-vpc.git?ref=master")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "resolve remote git failed: %s", string(out))

	// Must produce valid JSON with module tree
	var result map[string]any
	require.NoError(t, json.Unmarshal(out, &result), "output should be valid JSON")
	assert.Contains(t, result, "modules", "JSON output should contain 'modules' key")
}

// TestBinary_BundleWithSubpath verifies that bundle handles subpath sources correctly.
// A subpath source like "git::https://github.com/user/repo.git//modules/vpc" should
// bundle only the modules/vpc subdirectory as the root module.
func TestBinary_BundleWithSubpath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test with network access")
	}

	binary := buildBinary(t)
	outputPath := filepath.Join(t.TempDir(), "bundle.zip")

	// Bundle a subpath of a remote git module
	cmd := exec.Command(binary, "bundle", //nolint:gosec // G204: subprocess is intentional for testing binary
		"git::https://github.com/terraform-aws-modules/terraform-aws-vpc.git//examples?ref=master", //nolint:lll // long URL
		outputPath)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "bundle subpath source failed: %s", string(out))

	// Verify bundle was created
	_, err = os.Stat(outputPath)
	require.NoError(t, err, "bundle file should exist")

	// Verify it's a valid zip
	err = validateZipFile(t, outputPath)
	require.NoError(t, err, "bundle should be a valid zip archive")
}
