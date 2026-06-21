//nolint:gosec,errcheck // test files use standard permissions and safe paths
package cmd

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/blang/tofupress/pkg/tofupress"
)

func TestRunBundleErrorsOnUnknownExtensionWhenFormatAuto(t *testing.T) {
	tmpDir := t.TempDir()
	cmd := &cobra.Command{}
	cmd.Flags().String("format", "auto", "")
	cmd.Flags().Bool("oci-compliant", false, "")
	cmd.Flags().String("strip", "module-dir", "")

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
	cmd.Flags().String("strip", "module-dir", "")

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
	assert.Contains(t, string(data), `"strip_mode": "module-dir"`)
}

func TestRunBundleDefaultsToModuleDirStripMode(t *testing.T) {
	sourceDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(sourceDir, "main.tf"), []byte(`output "name" { value = "root" }`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(sourceDir, "README.md"), []byte("strip"), 0o644))

	bundlePath := filepath.Join(t.TempDir(), "bundle.zip")

	cmd := newTestBundleCommand(t, "")
	require.NoError(t, runBundle(cmd, []string{sourceDir, bundlePath}))

	assert.Contains(t, zipFileNamesForCmdTest(t, bundlePath), "main.tf")
}

func TestRunBundleStripNoneKeepsReadme(t *testing.T) {
	sourceDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(sourceDir, "main.tf"), []byte(`output "name" { value = "root" }`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(sourceDir, "README.md"), []byte("keep"), 0o644))
	bundlePath := filepath.Join(t.TempDir(), "bundle.zip")

	cmd := newTestBundleCommand(t, "")
	require.NoError(t, cmd.Flags().Set("strip", "none"))
	require.NoError(t, runBundle(cmd, []string{sourceDir, bundlePath}))

	assert.Contains(t, zipFileNamesForCmdTest(t, bundlePath), "README.md")
}

func TestRunBundleConfigOnlyWarnsForFilesystemReads(t *testing.T) {
	sourceDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(sourceDir, "main.tf"), []byte(`variable "name" { type = string }
locals { rendered = file("templates/${var.name}.tftpl") }`), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(sourceDir, "templates"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sourceDir, "templates", "x.tftpl"), []byte("strip"), 0o644))
	bundlePath := filepath.Join(t.TempDir(), "bundle.zip")

	cmd := newTestBundleCommand(t, "")
	require.NoError(t, cmd.Flags().Set("strip", "config-only"))
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)

	require.NoError(t, runBundle(cmd, []string{sourceDir, bundlePath}))
	assert.Contains(t, stderr.String(), "Warning:")
	assert.Contains(t, stderr.String(), "filesystem reads were detected")
}

func newTestBundleCommand(t *testing.T, metadataPath string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.Flags().String("format", "zip", "")
	cmd.Flags().Bool("oci-compliant", false, "")
	cmd.Flags().String("metadata-out", metadataPath, "")
	cmd.Flags().String("strip", string(tofupress.StripModeModuleDir), "")
	return cmd
}

func zipFileNamesForCmdTest(t *testing.T, archivePath string) []string {
	t.Helper()
	reader, err := zip.OpenReader(archivePath)
	require.NoError(t, err)
	defer reader.Close()

	var names []string
	for _, file := range reader.File {
		if !file.FileInfo().IsDir() {
			names = append(names, file.Name)
		}
	}
	return names
}
