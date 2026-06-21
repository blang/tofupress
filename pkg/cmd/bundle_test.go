//nolint:gosec // test files use standard permissions and safe paths
package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunBundleErrorsOnUnknownExtensionWhenFormatAuto(t *testing.T) {
	tmpDir := t.TempDir()
	cmd := &cobra.Command{}
	cmd.Flags().String("format", "auto", "")
	cmd.Flags().Bool("oci-compliant", false, "")

	err := runBundle(cmd, []string{tmpDir, filepath.Join(tmpDir, "bundle.unknown")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not infer bundle format")
	assert.Contains(t, err.Error(), "--format")
}

func TestRunBundleWritesMetadataOutAndPrintsStats(t *testing.T) {
	tmpDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "main.tf"), []byte(`output "name" { value = "root" }`), 0o644))

	outDir := t.TempDir()
	bundlePath := filepath.Join(outDir, "bundle.zip")
	metadataPath := filepath.Join(outDir, "meta.json")

	cmd := &cobra.Command{}
	cmd.Flags().String("format", "zip", "")
	cmd.Flags().Bool("oci-compliant", false, "")
	cmd.Flags().String("metadata-out", metadataPath, "")

	var stdout bytes.Buffer
	cmd.SetOut(&stdout)

	require.NoError(t, runBundle(cmd, []string{tmpDir, bundlePath}))

	assert.FileExists(t, bundlePath)
	assert.FileExists(t, metadataPath)
	assert.Contains(t, stdout.String(), "Module references:")
	assert.Contains(t, stdout.String(), "Unique packages:")
	assert.Contains(t, stdout.String(), "Metadata: "+metadataPath)

	data, err := os.ReadFile(metadataPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"schema_version": "1"`)
	assert.Contains(t, string(data), `"format": "zip"`)
}
