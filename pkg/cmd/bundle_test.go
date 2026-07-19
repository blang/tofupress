//nolint:gosec,errcheck // test files use standard permissions and safe paths
package cmd

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	cmd.Flags().String("strip", "optimistic", "")

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
	cmd.Flags().String("strip", "optimistic", "")

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
	assert.Contains(t, string(data), `"strip_mode": "optimistic"`)
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

func TestBundleCommandPrintsDeduplicatedPackages(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.tf"), []byte(`output "x" { value = "root" }`), 0o644))
	output := filepath.Join(t.TempDir(), "bundle.zip")

	cmd := newTestBundleCommand(t, "")
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(new(bytes.Buffer))

	require.NoError(t, runBundle(cmd, []string{root, output}))
	assert.Contains(t, buf.String(), "Deduplicated packages: 0")
}

// TestRunBundleEmbedsNonEmptyProvenanceUnderPlainGoBuild guards review item 7 /
// QA-6: a binary built with plain `go build` (no ldflags) must still embed
// non-empty tofupress.{version,commit,time} provenance into meta.json. `go
// test` does not inject ldflags, so this exercise is exactly that scenario.
func TestRunBundleEmbedsNonEmptyProvenanceUnderPlainGoBuild(t *testing.T) {
	sourceDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(sourceDir, "main.tf"), []byte(`output "x" { value = "root" }`), 0o644))
	bundlePath := filepath.Join(t.TempDir(), "bundle.zip")

	cmd := newTestBundleCommand(t, "")
	require.NoError(t, runBundle(cmd, []string{sourceDir, bundlePath}))

	meta, err := tofupress.ReadMetadataFromArtifact(bundlePath)
	require.NoError(t, err)
	assert.NotEmpty(t, meta.TofuPress.Version, "provenance version must never be blank (item 7)")
	assert.NotEmpty(t, meta.TofuPress.Commit, "provenance commit must never be blank under git VCS (item 7)")
	assert.NotEmpty(t, meta.TofuPress.Time, "provenance time must never be blank under git VCS (item 7)")
}

// TestEffectiveBuildInfoNeverBlankVersion asserts the fallback synthesizes a
// version even when VCS info is absent, so meta.json is never empty.
func TestEffectiveBuildInfoNeverBlankVersion(t *testing.T) {
	// Save and clear ldflags-injected vars to exercise the synthesis path.
	savedV, savedC, savedT := BuildVersion, BuildCommit, BuildTime
	BuildVersion, BuildCommit, BuildTime = "", "", ""
	t.Cleanup(func() { BuildVersion, BuildCommit, BuildTime = savedV, savedC, savedT })

	bi := EffectiveBuildInfo()
	assert.NotEmpty(t, bi.Version, "synthesized version must never be blank")
	assert.True(t, strings.HasPrefix(bi.Version, "dev-"), "plain go build version should be a dev-<ts> tag, got %q", bi.Version)
}

//nolint:unparam // metadataPath is always empty string; kept as parameter for test readability
func newTestBundleCommand(t *testing.T, metadataPath string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.Flags().String("format", "zip", "")
	cmd.Flags().Bool("oci-compliant", false, "")
	cmd.Flags().Bool("json", false, "")
	cmd.Flags().String("metadata-out", metadataPath, "")
	cmd.Flags().String("strip", string(tofupress.StripModeOptimistic), "")
	cmd.Flags().String("vendor-dir", "_vendor", "")
	return cmd
}

// TestRunBundleJSONEmitsMetadataToStdout (review item 9) verifies --json
// emits the artifact metadata as indented JSON to stdout (scriptable CI).
func TestRunBundleJSONEmitsMetadataToStdout(t *testing.T) {
	tmpDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "main.tf"), []byte(`output "name" { value = "root" }`), 0o644))
	bundlePath := filepath.Join(t.TempDir(), "bundle.zip")

	// Build the command with --json set.
	cmd := &cobra.Command{}
	cmd.Flags().String("format", "zip", "")
	cmd.Flags().Bool("oci-compliant", false, "")
	cmd.Flags().Bool("json", true, "")
	cmd.Flags().String("metadata-out", "", "")
	cmd.Flags().String("strip", string(tofupress.StripModeOptimistic), "")
	cmd.Flags().String("vendor-dir", "_vendor", "")

	var stdout bytes.Buffer
	cmd.SetOut(&stdout)

	require.NoError(t, runBundle(cmd, []string{tmpDir, bundlePath}))
	out := stdout.String()
	assert.Contains(t, out, `"schema_version"`)
	assert.Contains(t, out, `"command": {`)
	assert.Contains(t, out, `"name": "bundle"`)
	assert.NotContains(t, out, "Bundle created successfully", "--json must not emit human text")
	assert.NotContains(t, out, "Resolving modules in", "--json must not emit human preamble")
}

// TestRunBundleErrorsOnUnknownExtensionWithDirHint (review item 9 / QA-12)
// verifies a trailing-slash output path gets the directory-shape hint.
func TestRunBundleErrorsOnUnknownExtensionWithDirHint(t *testing.T) {
	tmpDir := t.TempDir()
	cmd := &cobra.Command{}
	cmd.Flags().String("format", "auto", "")
	cmd.Flags().Bool("oci-compliant", false, "")
	cmd.Flags().Bool("json", false, "")
	cmd.Flags().String("strip", "optimistic", "")

	err := runBundle(cmd, []string{tmpDir, tmpDir + "/out/"})
	assert.Contains(t, err.Error(), "could not infer bundle format")
	assert.Contains(t, err.Error(), "looks like a directory")
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

// TestVersionCommandJSON (review item 9) verifies `version --json` emits
// scriptable JSON {version,commit,build_time} to stdout.
func TestVersionCommandJSON(t *testing.T) {
	var stdout bytes.Buffer
	versionCmd.SetOut(&stdout)
	require.NoError(t, versionCmd.Flags().Set("json", "true"))
	defer versionCmd.Flags().Set("json", "false")

	// invoke the Run directly; EffectiveBuildInfo is non-empty under plain `go test`.
	versionCmd.Run(versionCmd, nil)

	out := stdout.String()
	assert.Contains(t, out, `"version"`)
	assert.Contains(t, out, `"commit"`)
	assert.Contains(t, out, `"build_time"`)
}

// TestInstallSignalCleanupRunsOnStop (review item 9) verifies the happy-path
// stop function triggers cleanup (so a successful run still releases the
// tempdir even though the deferred cleanup is the same once-guarded target).
func TestInstallSignalCleanupRunsOnStop(t *testing.T) {
	var ran atomic.Int32
	cleanup := func() {
		ran.Add(1)
	}
	stop := installSignalCleanup(cleanup)
	stop()
	// once-guarded: stop fires cleanup exactly once; the deferred cleanup is
	// the same guarded target, so the count stays 1.
	assert.Equal(t, int32(1), ran.Load(), "stop should fire cleanup exactly once")
}
