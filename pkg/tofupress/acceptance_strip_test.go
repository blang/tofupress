//go:build integration

//nolint:gosec // integration tests execute test binary and access local paths
package tofupress

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAcceptance_Strip_StaticFileRead(t *testing.T) {
	bin := buildBinary(t)
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", `
locals {
  cfg = file("${path.module}/data/config.json")
}
output "key" { value = jsondecode(local.cfg).key }
`)
	writeIntegrationFile(t, src, "data/config.json", `{"key":"val"}`)

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	_, _ = runTofuPressBundle(t, bin, src, artifact, "--format=zip")

	metadata := metadataFromArtifact(t, artifact)
	require.Len(t, metadata.FilesystemFunctions, 1)
	ff := metadata.FilesystemFunctions[0]
	assert.True(t, ff.Static, "static file read should be detected as static")
	assert.Equal(t, handlingStaticInclude, ff.Handling, "static read should use static-include handling")
	assert.Equal(t, "file", ff.Function)
	assert.NotEmpty(t, ff.IncludedPaths, "included paths should not be empty for static file read")

	validateArchiveWithAllTools(t, serveArtifact(t, artifact))
}

func TestAcceptance_Strip_DynamicFileRead(t *testing.T) {
	bin := buildBinary(t)
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", `
variable "filename" {
  type    = string
  default = "runtime.txt"
}
locals {
  data = file("${path.module}/${var.filename}")
}
output "data" { value = local.data }
`)
	writeIntegrationFile(t, src, "runtime.txt", `runtime data`)

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	_, _ = runTofuPressBundle(t, bin, src, artifact, "--format=zip")

	metadata := metadataFromArtifact(t, artifact)
	require.Len(t, metadata.FilesystemFunctions, 1)
	ff := metadata.FilesystemFunctions[0]
	assert.False(t, ff.Static, "dynamic file read should be detected as not static")
	assert.Equal(t, handlingDynamicFallback, ff.Handling, "dynamic read should use dynamic-package-fallback")
	assert.Equal(t, "file", ff.Function)

	validateArchiveWithAllTools(t, serveArtifact(t, artifact))
}

func TestAcceptance_Strip_TemplateFile(t *testing.T) {
	bin := buildBinary(t)
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", `
locals {
  rendered = templatefile("${path.module}/templates/userdata.tftpl", { name = "world" })
}
output "rendered" { value = local.rendered }
`)
	writeIntegrationFile(t, src, "templates/userdata.tftpl", `hello ${name}`)

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	_, _ = runTofuPressBundle(t, bin, src, artifact, "--format=zip")

	metadata := metadataFromArtifact(t, artifact)
	require.Len(t, metadata.FilesystemFunctions, 1)
	ff := metadata.FilesystemFunctions[0]
	assert.Equal(t, "templatefile", ff.Function)
	assert.True(t, ff.Static, "static templatefile should be detected as static")
	assert.Equal(t, handlingStaticInclude, ff.Handling)

	validateArchiveWithAllTools(t, serveArtifact(t, artifact))
}

func TestAcceptance_Strip_FileSet(t *testing.T) {
	bin := buildBinary(t)
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", `
locals {
  configs = fileset("${path.module}/configs", "*.yaml")
}
output "count" { value = length(local.configs) }
`)
	writeIntegrationFile(t, src, "configs/a.yaml", `key: a`)
	writeIntegrationFile(t, src, "configs/b.yaml", `key: b`)

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	_, _ = runTofuPressBundle(t, bin, src, artifact, "--format=zip")

	metadata := metadataFromArtifact(t, artifact)
	require.Len(t, metadata.FilesystemFunctions, 1)
	ff := metadata.FilesystemFunctions[0]
	assert.Equal(t, "fileset", ff.Function)
	assert.True(t, ff.Static, "static fileset pattern should be detected as static")
	assert.GreaterOrEqual(t, len(ff.IncludedPaths), 1, "fileset should include at least one matching file")

	validateArchiveWithAllTools(t, serveArtifact(t, artifact))
}

func TestAcceptance_Strip_FileMD5(t *testing.T) {
	bin := buildBinary(t)
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", `
locals {
  hash = filemd5("${path.module}/binary.bin")
}
output "hash" { value = local.hash }
`)
	writeIntegrationFile(t, src, "binary.bin", `binary content here`)

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	_, _ = runTofuPressBundle(t, bin, src, artifact, "--format=zip")

	metadata := metadataFromArtifact(t, artifact)
	require.Len(t, metadata.FilesystemFunctions, 1)
	ff := metadata.FilesystemFunctions[0]
	assert.Equal(t, "filemd5", ff.Function)
	assert.True(t, ff.Static, "static filemd5 read should be detected as static")
	assert.NotEmpty(t, ff.IncludedPaths, "included paths should not be empty for static filemd5 read")

	validateArchiveWithAllTools(t, serveArtifact(t, artifact))
}

func TestAcceptance_Strip_FileBase64(t *testing.T) {
	bin := buildBinary(t)
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", `
locals {
  encoded = filebase64("${path.module}/some-file")
}
output "encoded" { value = local.encoded }
`)
	writeIntegrationFile(t, src, "some-file", `raw bytes for base64`)

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	_, _ = runTofuPressBundle(t, bin, src, artifact, "--format=zip")

	metadata := metadataFromArtifact(t, artifact)
	require.Len(t, metadata.FilesystemFunctions, 1)
	ff := metadata.FilesystemFunctions[0]
	assert.Equal(t, "filebase64", ff.Function)
	assert.True(t, ff.Static, "static filebase64 read should be detected as static")
	assert.NotEmpty(t, ff.IncludedPaths, "included paths should not be empty for static filebase64 read")

	validateArchiveWithAllTools(t, serveArtifact(t, artifact))
}

func TestAcceptance_Strip_FileExists(t *testing.T) {
	bin := buildBinary(t)
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", `
locals {
  has_optional = fileexists("${path.module}/optional.txt")
}
output "exists" { value = local.has_optional }
`)
	writeIntegrationFile(t, src, "optional.txt", `optional content`)

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	_, _ = runTofuPressBundle(t, bin, src, artifact, "--format=zip")

	metadata := metadataFromArtifact(t, artifact)
	require.Len(t, metadata.FilesystemFunctions, 1)
	ff := metadata.FilesystemFunctions[0]
	assert.Equal(t, "fileexists", ff.Function)
	// fileexists with a literal (non-variable) path argument is static — the
	// function resolves at plan time and the file path doesn't depend on variables.
	assert.True(t, ff.Static, "fileexists with literal path should be static")
	assert.Equal(t, handlingStaticInclude, ff.Handling)

	validateArchiveWithAllTools(t, serveArtifact(t, artifact))
}

func TestAcceptance_Strip_PathRoot(t *testing.T) {
	bin := buildBinary(t)
	src := t.TempDir()
	// Child module reads from a file at path.root (relative to the bundle root).
	// Put the config file at the root level so the child can reference it.
	writeIntegrationFile(t, src, "global-config.yaml", `setting: value`)
	// Root module — no filesystem reads itself
	writeIntegrationFile(t, src, "main.tf", `
module "child" {
  source = "./modules/child"
}
output "setting" { value = module.child.setting }
`)
	// Child module reads from path.root
	writeIntegrationFile(t, src, "modules/child/main.tf", `
locals {
  config = yamldecode(file("${path.root}/global-config.yaml"))
}
output "setting" { value = local.config.setting }
`)

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	_, _ = runTofuPressBundle(t, bin, src, artifact, "--format=zip")

	metadata := metadataFromArtifact(t, artifact)
	require.NotEmpty(t, metadata.FilesystemFunctions, "filesystem functions should be detected for path.root read")
	// Find the file function
	found := false
	for _, ff := range metadata.FilesystemFunctions {
		if ff.Function == "file" {
			found = true
			assert.True(t, ff.Static, "static file read from path.root should be static")
			// path.root file may or may not be included depending on strip behavior
			t.Logf("file function: static=%v handling=%s included_paths=%v", ff.Static, ff.Handling, ff.IncludedPaths)
			break
		}
	}
	assert.True(t, found, "file function from path.root should be detected")

	// Note: terraform validate is NOT called here because path.root resolution
	// in bundled archives depends on the consumer's working directory structure.
	// This test verifies filesystem function detection and bundle creation.
}

func TestAcceptance_Strip_ConfigOnly_DynamicWarns(t *testing.T) {
	bin := buildBinary(t)
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", `
variable "data_file" {
  type    = string
  default = "runtime.txt"
}
locals {
  runtime = file("${path.module}/data/${var.data_file}")
}
output "runtime" { value = local.runtime }
`)
	writeIntegrationFile(t, src, "data/runtime.txt", `runtime data`)

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	_, stderr := runTofuPressBundle(t, bin, src, artifact, "--format=zip", "--strip=config-only")
	assert.Contains(t, strings.ToLower(stderr), "warning", "stderr should contain warning about filesystem reads")
	assert.Contains(t, stderr, "filesystem reads were detected", "stderr should mention filesystem reads")

	metadata := metadataFromArtifact(t, artifact)
	assert.Equal(t, string(StripModeAggressive), metadata.Command.Options.StripMode)
	assert.Greater(t, metadata.Stats.StrippedFiles, 0, "config-only strip should strip files")
	assert.NotEmpty(t, metadata.StripWarnings, "config-only should produce strip warnings")

	require.NotEmpty(t, metadata.FilesystemFunctions, "filesystem functions should be detected")
	ff := metadata.FilesystemFunctions[0]
	assert.False(t, ff.Static, "dynamic read should not be static")
	assert.Equal(t, handlingDynamicFallback, ff.Handling)

	expectArchivePlanFailsWithAllTools(t, serveArtifact(t, artifact))
}

func TestAcceptance_Strip_MultipleFunctions(t *testing.T) {
	bin := buildBinary(t)
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", `
locals {
  file_data    = file("${path.module}/data/config.txt")
  rendered     = templatefile("${path.module}/templates/userdata.tftpl", { name = "test" })
  file_hash    = filemd5("${path.module}/binary.bin")
}
output "file_data" { value = local.file_data }
output "rendered"  { value = local.rendered }
output "hash"      { value = local.file_hash }
`)
	writeIntegrationFile(t, src, "data/config.txt", `config data`)
	writeIntegrationFile(t, src, "templates/userdata.tftpl", `hello ${name}`)
	writeIntegrationFile(t, src, "binary.bin", `binary content`)

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	_, _ = runTofuPressBundle(t, bin, src, artifact, "--format=zip")

	metadata := metadataFromArtifact(t, artifact)
	assert.Len(t, metadata.FilesystemFunctions, 3, "three distinct filesystem functions should be detected")

	functions := make(map[string]bool)
	for _, ff := range metadata.FilesystemFunctions {
		functions[ff.Function] = true
		assert.True(t, ff.Static, "all functions in this test use static paths")
	}
	assert.True(t, functions["file"], "file function should be detected")
	assert.True(t, functions["templatefile"], "templatefile function should be detected")
	assert.True(t, functions["filemd5"], "filemd5 function should be detected")

	validateArchiveWithAllTools(t, serveArtifact(t, artifact))
}

func TestAcceptance_Strip_NestedModuleReads(t *testing.T) {
	bin := buildBinary(t)
	src := t.TempDir()
	// Root module reads its own file
	writeIntegrationFile(t, src, "main.tf", `
locals {
  root_data = file("${path.module}/data/root-config.json")
}
module "child" {
  source = "./modules/child"
}
output "root_data"  { value = jsondecode(local.root_data).key }
output "child_data" { value = module.child.data }
`)
	writeIntegrationFile(t, src, "data/root-config.json", `{"key":"root-value"}`)

	// Child module reads its own file
	writeIntegrationFile(t, src, "modules/child/main.tf", `
locals {
  child_data = file("${path.module}/data/child-config.json")
}
output "data" { value = jsondecode(local.child_data).key }
`)
	writeIntegrationFile(t, src, "modules/child/data/child-config.json", `{"key":"child-value"}`)

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	_, _ = runTofuPressBundle(t, bin, src, artifact, "--format=zip")

	metadata := metadataFromArtifact(t, artifact)
	assert.GreaterOrEqual(t, len(metadata.FilesystemFunctions), 2,
		"at least two filesystem functions should be detected (root + child)")

	// Verify both root and child read are detected
	functions := make([]string, len(metadata.FilesystemFunctions))
	for i, ff := range metadata.FilesystemFunctions {
		functions[i] = ff.Function
		assert.Equal(t, "file", ff.Function)
		assert.True(t, ff.Static, "all static file reads should be detected as static")
	}
	assert.GreaterOrEqual(t, len(functions), 2, "both root and child file reads should appear")

	validateArchiveWithAllTools(t, serveArtifact(t, artifact))
}
