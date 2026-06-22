//go:build integration

//nolint:gosec // integration tests execute test binary
package tofupress

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAcceptance_Strip_Default verifies the default strip mode (module-dir)
// strips non-config files (README) but preserves static filesystem reads.
func TestAcceptance_Strip_Default(t *testing.T) {
	bin := buildBinary(t)
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", `
locals {
  data = file("${path.module}/data/config.json")
}
output "config" { value = jsondecode(local.data) }
`)
	writeIntegrationFile(t, src, "data/config.json", `{"key": "value"}`)
	writeIntegrationFile(t, src, "README.md", `# My Module`)

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, src, artifact, "--format=zip")

	metadata := metadataFromArtifact(t, artifact)
	assert.Equal(t, string(StripModeModuleDir), metadata.Command.Options.StripMode)
	assert.Greater(t, metadata.Stats.StrippedFiles, 0)
	assert.Greater(t, metadata.Stats.StrippedBytes, int64(0))

	require.NotEmpty(t, metadata.FilesystemFunctions)
	assert.True(t, metadata.FilesystemFunctions[0].Static)

	validateArchiveWithAllTools(t, serveArtifact(t, artifact))
}

// TestAcceptance_Strip_None verifies --strip=none keeps all files.
func TestAcceptance_Strip_None(t *testing.T) {
	bin := buildBinary(t)
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", `output "hello" { value = "world" }`)
	writeIntegrationFile(t, src, "README.md", `# README`)

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, src, artifact, "--format=zip", "--strip=none")

	metadata := metadataFromArtifact(t, artifact)
	assert.Equal(t, string(StripModeNone), metadata.Command.Options.StripMode)
	assert.Equal(t, 0, metadata.Stats.StrippedFiles)
}

// TestAcceptance_Strip_ConfigOnly verifies --strip=config-only strips non-config files.
func TestAcceptance_Strip_ConfigOnly(t *testing.T) {
	bin := buildBinary(t)
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", `output "hello" { value = "world" }`)
	writeIntegrationFile(t, src, "README.md", `# README`)

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, src, artifact, "--format=zip", "--strip=config-only")

	metadata := metadataFromArtifact(t, artifact)
	assert.Equal(t, string(StripModeConfigOnly), metadata.Command.Options.StripMode)
	assert.Greater(t, metadata.Stats.StrippedFiles, 0)
}

// TestAcceptance_OCICompliant verifies --oci-compliant produces a valid artifact
// with OCI-compliant metadata.
func TestAcceptance_OCICompliant(t *testing.T) {
	bin := buildBinary(t)
	repo := createLocalGitModuleRepo(t, "oci-dep", map[string]string{
		"main.tf": `output "remote" { value = "ok" }`,
	})
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", fmt.Sprintf(`
module "dep" {
  source = %q
}
`, gitFileSource(repo)))

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, src, artifact, "--oci-compliant")

	metadata := metadataFromArtifact(t, artifact)
	assert.True(t, metadata.Command.Options.OCICompliant)

	validateArchiveWithAllTools(t, serveArtifact(t, artifact))
}

// TestAcceptance_OCICompliant_RequiresZip verifies --oci-compliant with
// non-zip format produces an error.
func TestAcceptance_OCICompliant_RequiresZip(t *testing.T) {
	bin := buildBinary(t)
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", `output "hello" { value = "world" }`)

	artifact := filepath.Join(t.TempDir(), "bundle.tar.gz")

	cmd := exec.Command(bin, "bundle", src, artifact, "--oci-compliant", "--format=tar.gz") //nolint:gosec
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "oci-compliant with tar.gz should fail")
	assert.True(t,
		strings.Contains(strings.ToLower(string(out)), "oci-compliant requires zip") ||
			strings.Contains(strings.ToLower(string(out)), "requires zip"),
		"error should mention oci-compliant requires zip, got: %s", string(out))
}

// TestAcceptance_VendorDir_Custom verifies --vendor-dir flag produces artifact
// with packages in the custom directory.
func TestAcceptance_VendorDir_Custom(t *testing.T) {
	bin := buildBinary(t)
	repo := createLocalGitModuleRepo(t, "remote", map[string]string{
		"main.tf": `output "remote" { value = "ok" }`,
	})
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", fmt.Sprintf(`
module "remote" {
  source = %q
}
`, gitFileSource(repo)))

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	stdout, _ := runTofuPressBundle(t, bin, src, artifact, "--format=zip", "--vendor-dir=custom_vendor")
	// Bundle succeeded with custom vendor dir.
	assert.Contains(t, stdout, "Bundle created")

	validateArchiveWithAllTools(t, serveArtifact(t, artifact))
}

// TestAcceptance_VendorDir_ConflictRejected verifies that a conflict between
// user local modules under modules/ and remote packages is detected and rejected.
func TestAcceptance_VendorDir_ConflictRejected(t *testing.T) {
	bin := buildBinary(t)
	repo := createLocalGitModuleRepo(t, "remote", map[string]string{
		"main.tf": `output "remote" { value = "ok" }`,
	})
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", fmt.Sprintf(`
module "local" {
  source = "./modules/local"
}
module "remote" {
  source = %q
}
`, gitFileSource(repo)))
	writeIntegrationFile(t, src, "modules/local/main.tf", `# local module`)

	artifact := filepath.Join(t.TempDir(), "bundle.zip")

	cmd := exec.Command(bin, "bundle", src, artifact, "--format=zip") //nolint:gosec
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "vendor dir conflict should cause error")
	assert.Contains(t, strings.ToLower(string(out)), "conflict")
}

// TestAcceptance_VendorDir_ConflictResolved verifies that a custom vendor dir
// avoids the conflict and preserves both local modules and remote packages.
func TestAcceptance_VendorDir_ConflictResolved(t *testing.T) {
	bin := buildBinary(t)
	repo := createLocalGitModuleRepo(t, "remote", map[string]string{
		"main.tf": `output "remote" { value = "ok" }`,
	})
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", fmt.Sprintf(`
module "local" {
  source = "./modules/local"
  input  = "hello"
}
module "remote" {
  source = %q
}
`, gitFileSource(repo)))
	writeIntegrationFile(t, src, "modules/local/main.tf", `
variable "input" { type = string }
output "out" { value = var.input }
`)

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, src, artifact, "--format=zip", "--vendor-dir=_vendor")

	validateArchiveWithAllTools(t, serveArtifact(t, artifact))
}

// TestAcceptance_MetadataOut verifies --metadata-out writes metadata to an external file.
func TestAcceptance_MetadataOut(t *testing.T) {
	bin := buildBinary(t)
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", `output "hello" { value = "world" }`)

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	metadataPath := filepath.Join(t.TempDir(), "external-meta.json")
	runTofuPressBundle(t, bin, src, artifact, "--format=zip", "--metadata-out", metadataPath)

	// Verify external file exists and is valid JSON
	require.FileExists(t, metadataPath)
	data, err := os.ReadFile(metadataPath) //nolint:gosec
	require.NoError(t, err)

	var meta map[string]any
	require.NoError(t, json.Unmarshal(data, &meta))
	assert.Equal(t, "1", meta["schema_version"])
}

// TestAcceptance_MetadataOut_OCI verifies --metadata-out with --oci-compliant
// writes metadata externally and embeds matching metadata in the artifact.
func TestAcceptance_MetadataOut_OCI(t *testing.T) {
	bin := buildBinary(t)
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", `output "oci_meta" { value = "test" }`)

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	metadataPath := filepath.Join(t.TempDir(), "oci-meta.json")
	runTofuPressBundle(t, bin, src, artifact, "--oci-compliant", "--metadata-out", metadataPath)

	// Verify external file exists
	require.FileExists(t, metadataPath)
	extData, err := os.ReadFile(metadataPath) //nolint:gosec
	require.NoError(t, err)

	var extMeta map[string]any
	require.NoError(t, json.Unmarshal(extData, &extMeta))
	assert.Equal(t, "1", extMeta["schema_version"])

	// Verify embedded metadata matches external
	embedded := metadataFromArtifact(t, artifact)
	assert.True(t, embedded.Command.Options.OCICompliant)
	assert.Equal(t, "1", embedded.SchemaVersion)
}
