package tofupress

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCLI_ResolveCommand(t *testing.T) {
	// Create a test module structure
	tmpDir := t.TempDir()
	writeTerraformFile(t, tmpDir, "main.tf", `
module "local" {
  source = "./modules/local"
}
`)
	localDir := filepath.Join(tmpDir, "modules", "local")
	require.NoError(t, os.MkdirAll(localDir, 0755))
	writeTerraformFile(t, localDir, "main.tf", `# local module`)

	// Run resolve command
	var stdout, stderr bytes.Buffer
	err := RunCLI([]string{"resolve", tmpDir}, &stdout, &stderr)
	require.NoError(t, err)

	// Verify output contains module information
	output := stdout.String()
	assert.Contains(t, output, "local")
	assert.Contains(t, output, "Module tree")
}

func TestCLI_ResolveCommandJSON(t *testing.T) {
	// Create a test module structure
	tmpDir := t.TempDir()
	writeTerraformFile(t, tmpDir, "main.tf", `
module "local" {
  source = "./modules/local"
}
`)
	localDir := filepath.Join(tmpDir, "modules", "local")
	require.NoError(t, os.MkdirAll(localDir, 0755))
	writeTerraformFile(t, localDir, "main.tf", `# local module`)

	// Run resolve command with JSON output
	var stdout, stderr bytes.Buffer
	err := RunCLI([]string{"resolve", "--json", tmpDir}, &stdout, &stderr)
	require.NoError(t, err)

	// Verify output is valid JSON
	var result map[string]any
	err = json.Unmarshal(stdout.Bytes(), &result)
	require.NoError(t, err)

	// Verify JSON contains expected fields
	assert.Contains(t, result, "root")
	assert.Contains(t, result, "modules")
	assert.Contains(t, result, "packages")
}

func TestCLI_BundleCommand(t *testing.T) {
	// Create a test module structure
	tmpDir := t.TempDir()
	writeTerraformFile(t, tmpDir, "main.tf", `
module "local" {
  source = "./modules/local"
}
`)
	localDir := filepath.Join(tmpDir, "modules", "local")
	require.NoError(t, os.MkdirAll(localDir, 0755))
	writeTerraformFile(t, localDir, "main.tf", `# local module`)

	// Create output path
	outputDir := t.TempDir()
	outputPath := filepath.Join(outputDir, "bundle.tar.gz")

	// Run bundle command
	var stdout, stderr bytes.Buffer
	err := RunCLI([]string{"bundle", tmpDir, outputPath}, &stdout, &stderr)
	require.NoError(t, err)

	// Verify bundle was created
	assert.FileExists(t, outputPath)

	// Verify bundle is not empty
	info, err := os.Stat(outputPath)
	require.NoError(t, err)
	assert.Greater(t, info.Size(), int64(0))
}

func TestCLI_ResolveCommandNonExistentDir(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := RunCLI([]string{"resolve", "/nonexistent/path"}, &stdout, &stderr)
	assert.Error(t, err)
}

func TestCLI_BundleCommandMissingArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := RunCLI([]string{"bundle"}, &stdout, &stderr)
	assert.Error(t, err)
}

func TestCLI_NoCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// Cobra shows help by default (no error)
	err := RunCLI([]string{}, &stdout, &stderr)
	require.NoError(t, err)

	output := stdout.String()
	assert.Contains(t, output, "tofupress")
}

func TestCLI_Help(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := RunCLI([]string{"--help"}, &stdout, &stderr)
	require.NoError(t, err)

	output := stdout.String()
	assert.Contains(t, output, "tofupress")
	assert.Contains(t, output, "resolve")
	assert.Contains(t, output, "bundle")
}
