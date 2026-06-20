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

// testBinaryPath holds the path to the pre-compiled test binary.
// Built once in TestMain to avoid recompilation per test.
var testBinaryPath string

// TestMain builds the binary once before all tests to avoid per-test recompilation.
func TestMain(m *testing.M) {
	// Create a shared temp directory for the binary
	testDir, err := os.MkdirTemp("", "tofupress-test-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create test temp dir: %v\n", err) //nolint:errcheck // stderr is best-effort
		os.Exit(1)
	}
	defer os.RemoveAll(testDir) //nolint:errcheck,gosec // cleanup failures are acceptable

	// Build binary once
	binaryName := "tofupress"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	testBinaryPath = filepath.Join(testDir, binaryName)

	// Get the project root directory (3 levels up from this test file)
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		fmt.Fprintln(os.Stderr, "failed to determine test file location")
		os.Exit(1)
	}
	projectRoot := filepath.Dir(filepath.Dir(filepath.Dir(filename)))

	cmd := exec.Command("go", "build", "-o", testBinaryPath, "./cmd/tofupress") //nolint:gosec // G204: subprocess is intentional for building test binary
	cmd.Dir = projectRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to build test binary: %v\n%s\n", err, output) //nolint:errcheck // stderr is best-effort
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// buildBinary returns the path to the pre-compiled test binary.
// The binary is built once in TestMain to avoid recompilation per test.
func buildBinary(t *testing.T) string {
	t.Helper()
	require.NotEmpty(t, testBinaryPath, "test binary path should be set by TestMain")
	_, err := os.Stat(testBinaryPath)
	require.NoError(t, err, "test binary should exist at %s", testBinaryPath)
	return testBinaryPath
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

// validateZipContents opens a zip archive and verifies it contains all expected file paths.
// Returns a map of file path -> content for further validation.
func validateZipContents(t *testing.T, path string, expectedFiles []string) (map[string]string, error) {
	t.Helper()

	f, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open zip: %w", err)
	}
	defer f.Close() //nolint:errcheck // best effort

	// Build map of all file paths and their contents
	contents := make(map[string]string)
	for _, file := range f.File {
		if file.FileInfo().IsDir() {
			continue
		}
		rc, readErr := file.Open()
		if readErr != nil {
			return nil, fmt.Errorf("failed to read zip entry %s: %w", file.Name, readErr)
		}
		data, readErr := io.ReadAll(rc)
		rc.Close() //nolint:errcheck // best effort
		if readErr != nil {
			return nil, fmt.Errorf("failed to read zip entry content %s: %w", file.Name, readErr)
		}
		contents[file.Name] = string(data)
	}

	// Check all expected files are present
	for _, expected := range expectedFiles {
		found := false
		for name := range contents {
			if name == expected {
				found = true
				break
			}
		}
		if !found {
			return contents, fmt.Errorf("expected file %q not found in archive. Files present: %v", expected, zipFileNames(f.File))
		}
	}

	return contents, nil
}

// zipFileNames returns a slice of file names from zip entries.
func zipFileNames(files []*zip.File) []string {
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.Name
	}
	return names
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

// validateTarGzContents opens a tar.gz archive and verifies it contains all expected file paths.
// Returns a map of file path -> content for further validation.
func validateTarGzContents(t *testing.T, path string, expectedFiles []string) (map[string]string, error) {
	t.Helper()

	f, err := os.Open(path) //nolint:gosec // G304: path comes from test
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	defer f.Close() //nolint:errcheck // best effort

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gz.Close() //nolint:errcheck // best effort

	tr := tar.NewReader(gz)

	// Build map of all file paths and their contents
	contents := make(map[string]string)
	var fileNames []string
	for {
		header, readErr := tr.Next()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("failed to read tar entry: %w", readErr)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		data, readErr := io.ReadAll(tr)
		if readErr != nil {
			return nil, fmt.Errorf("failed to read tar entry content %s: %w", header.Name, readErr)
		}
		contents[header.Name] = string(data)
		fileNames = append(fileNames, header.Name)
	}

	// Check all expected files are present
	for _, expected := range expectedFiles {
		found := false
		for name := range contents {
			if name == expected {
				found = true
				break
			}
		}
		if !found {
			return contents, fmt.Errorf("expected file %q not found in archive. Files present: %v", expected, fileNames)
		}
	}

	return contents, nil
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

	// Deep validation: verify specific files are present
	contents, err := validateZipContents(t, outputPath, []string{"main.tf", "child/main.tf"})
	require.NoError(t, err, "bundle should contain expected files")

	// Verify main.tf contains the module reference
	mainContent := contents["main.tf"]
	assert.Contains(t, mainContent, "module \"child\"", "main.tf should contain child module reference")
	assert.Contains(t, mainContent, "source = \"./child\"", "main.tf should reference child module")

	// Verify child/main.tf exists and has content
	childContent := contents["child/main.tf"]
	assert.Contains(t, childContent, "# child module", "child/main.tf should contain child module content")
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

	// Deep validation: verify specific files are present in tar.gz
	contents, err := validateTarGzContents(t, outputPath, []string{"main.tf", "child/main.tf"})
	require.NoError(t, err, "tar.gz bundle should contain expected files")

	// Verify main.tf contains the module reference
	mainContent := contents["main.tf"]
	assert.Contains(t, mainContent, "module \"child\"", "main.tf should contain child module reference")
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

// TestBinary_BundleNestedArchive verifies that bundling a directory that was itself
// created by bundling another module works correctly. The sourcetree/ directory
// should be preserved in the new bundle.
func TestBinary_BundleNestedArchive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test with network access")
	}

	binary := buildBinary(t)

	// Create a fixture with a remote module
	fixtureDir := t.TempDir()
	mainTf := filepath.Join(fixtureDir, "main.tf")
	err := os.WriteFile(mainTf, []byte(`
module "remote" {
  source = "git::https://github.com/terraform-aws-modules/terraform-aws-vpc.git?ref=v5.0.0"
  name   = "test"
}
`), 0644)
	require.NoError(t, err)

	// First bundle
	firstBundlePath := filepath.Join(t.TempDir(), "first-bundle.zip")
	cmd := exec.Command(binary, "bundle", fixtureDir, firstBundlePath) //nolint:gosec // G204: subprocess is intentional
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "first bundle failed: %s", string(output))
	require.FileExists(t, firstBundlePath)

	// Extract the first bundle
	extractDir := t.TempDir()
	cmd = exec.Command("unzip", "-q", firstBundlePath, "-d", extractDir)
	output, err = cmd.CombinedOutput()
	require.NoError(t, err, "unzip failed: %s", string(output))

	// Verify the extracted bundle has sourcetree/
	_, err = os.Stat(filepath.Join(extractDir, "sourcetree"))
	require.NoError(t, err, "extracted bundle should have sourcetree/ directory")

	// Second bundle (bundle the extracted bundle)
	secondBundlePath := filepath.Join(t.TempDir(), "second-bundle.zip")
	cmd = exec.Command(binary, "bundle", extractDir, secondBundlePath) //nolint:gosec // G204: subprocess is intentional
	output, err = cmd.CombinedOutput()
	require.NoError(t, err, "second bundle failed: %s", string(output))
	require.FileExists(t, secondBundlePath)

	// Verify the second bundle also contains sourcetree/
	cmd = exec.Command("unzip", "-l", secondBundlePath)
	output, err = cmd.CombinedOutput()
	require.NoError(t, err, "unzip -l failed: %s", string(output))
	require.Contains(t, string(output), "sourcetree/", "second bundle should contain sourcetree/ directory")
	require.Contains(t, string(output), "main.tf", "second bundle should contain main.tf")
}
