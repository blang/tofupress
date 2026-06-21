//go:build integration

//nolint:gosec // integration tests use temporary directories and local loopback URLs
package tofupress

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntegrationTerraformOpenTofuValidateStaticBundleArchives(t *testing.T) {
	bin := buildTofuPressBinary(t)

	formats := []struct {
		name string
		ext  string
		flag string
	}{
		{name: "zip", ext: ".zip", flag: "--format=zip"},
		{name: "tar.gz", ext: ".tar.gz", flag: "--format=tar.gz"},
	}

	for _, tt := range formats {
		t.Run(tt.name, func(t *testing.T) {
			sourceDir := t.TempDir()
			writeIntegrationFile(t, sourceDir, "main.tf", `
locals {
  rendered = templatefile("${path.module}/templates/userdata.tftpl", { name = "world" })
}

module "child" {
  source = "./modules/child"
  name   = local.rendered
}

output "rendered" {
  value = module.child.name
}
`)
			writeIntegrationFile(t, sourceDir, "modules/child/main.tf", `
variable "name" { type = string }
output "name" { value = var.name }
`)
			writeIntegrationFile(t, sourceDir, "templates/userdata.tftpl", `hello ${name}`)
			writeIntegrationFile(t, sourceDir, "README.md", `stripped by safe default`)

			artifact := filepath.Join(t.TempDir(), "bundle"+tt.ext)
			metadataPath := filepath.Join(t.TempDir(), "meta.json")
			stdout, _ := runTofuPressBundle(t, bin, sourceDir, artifact, tt.flag, "--metadata-out", metadataPath)
			assert.Contains(t, stdout, "Stripped bytes:")

			metadata := metadataFromArtifact(t, artifact)
			assert.Equal(t, string(StripModeModuleDir), metadata.Command.Options.StripMode)
			assert.Greater(t, metadata.Stats.StrippedFiles, 0)
			assert.Greater(t, metadata.Stats.StrippedBytes, int64(0))
			require.Len(t, metadata.FilesystemFunctions, 1)
			assert.Equal(t, "templatefile", metadata.FilesystemFunctions[0].Function)

			validateArchiveWithAllTools(t, serveArtifact(t, artifact))
		})
	}
}
