//go:build integration

package tofupress

import (
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
	cmd := exec.Command("go", "build", "-o", binaryPath, "./cmd/tofupress")
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
	require.NoError(t, os.MkdirAll(childDir, 0755))
	writeTerraformFile(t, childDir, "main.tf", `# child module`)

	return fixtureDir
}

// TestBinary_HelpShowsCommands verifies that the CLI binary shows resolve and bundle commands in help.
// This is a RED phase test - it should FAIL because commands are not wired up yet.
func TestBinary_HelpShowsCommands(t *testing.T) {
	binary := buildBinary(t)

	// Run the binary with --help flag
	cmd := exec.Command(binary, "--help")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "binary --help should succeed: %s", string(output))

	outputStr := string(output)

	// These assertions should FAIL because resolve and bundle commands are not registered yet.
	// We check for "Available Commands" which only appears when subcommands exist,
	// and the exact indented command names (cobra uses "  name" format in the commands section).
	assert.Contains(t, outputStr, "Available Commands", "help output should have an Available Commands section")
	assert.Contains(t, outputStr, "  resolve", "help output should list 'resolve' as a command")
	assert.Contains(t, outputStr, "  bundle", "help output should list 'bundle' as a command")
}
