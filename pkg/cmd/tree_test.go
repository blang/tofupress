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

// newTestTreeCommand builds a cobra.Command wired with the `tree` flags, mirroring
// newTestModuleCommand for the module adapter. Uses the optimistic default strip
// level and the private vendor dir.
func newTestTreeCommand(t *testing.T, _ string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.Flags().String("format", "zip", "")
	cmd.Flags().Bool("oci-compliant", false, "")
	cmd.Flags().Bool("strict-oci", true, "")
	cmd.Flags().Bool("json", false, "")
	cmd.Flags().String("metadata-out", "", "")
	cmd.Flags().String("strip", string(tofupress.StripModeOptimistic), "")
	cmd.Flags().String("vendor-dir", "_vendor", "")
	return cmd
}

// TestRunTreeRefusesNoTfDirsAnywhere pins the ADR-0002 refusal for the `tree`
// adapter: a subject with no .tf/.tofu directories anywhere is refused (the
// resolver-side refusal surfaces through runPress as a resolve error). The
// error points the user at `tofupress module` (the single-module press).
func TestRunTreeRefusesNoTfDirsAnywhere(t *testing.T) {
	subject := t.TempDir() // docs only, no .tf anywhere
	require.NoError(t, os.MkdirAll(filepath.Join(subject, "docs"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(subject, "docs", "x.md"), []byte("md"), 0o644))

	cmd := newTestTreeCommand(t, "")
	err := runPressTree(cmd, []string{subject, filepath.Join(t.TempDir(), "out.zip")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "contains no .tf or .tofu directories")
	assert.Contains(t, err.Error(), "tofupress module")
}

// TestRunTreePreservesLayout pins ADR-0002's unpivoted contract: a `tree` press
// of a modules-only subject stages every .tf-dir at its subject-relative path
// (NO pivot to a single entry). The archive root contains the subject's
// directory shape verbatim; the consumer may address any anchor via //.
func TestRunTreePreservesLayout(t *testing.T) {
	subject := t.TempDir()
	writeNested := func(rel, body string) {
		p := filepath.Join(subject, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	}
	writeNested("modules/moda/main.tf", `output "a" { value = "a" }`)
	writeNested("modules/modb/main.tf", `module "dep" { source = "../modb-shared" }`)
	writeNested("modules/modb-shared/main.tf", `output "s" { value = 1 }`)

	artifact := filepath.Join(t.TempDir(), "tree.zip")
	cmd := newTestTreeCommand(t, "")
	require.NoError(t, runPressTree(cmd, []string{subject, artifact}))

	// The archive preserves the subject's directory shape as-is; no pivot flattened
	// modb to the archive root.
	names := readZipNames(t, artifact)
	assert.Contains(t, names, "modules/moda/main.tf", "moda staged at subject-relative path (no pivot)")
	assert.Contains(t, names, "modules/modb/main.tf", "modb staged at subject-relative path (no pivot)")
	assert.Contains(t, names, "modules/modb-shared/main.tf", "shared dir staged at path; ../modb-shared ref resolves verbatim")
}

// TestRunTreeJSONEmitsMetadataWithTreeCommand pins that `tree --json` emits
// metadata carrying Command.Name = "tree" (ADR-0002 traceability).
func TestRunTreeJSONEmitsMetadataWithTreeCommand(t *testing.T) {
	subject := t.TempDir()
	p := filepath.Join(subject, "modules", "m", "main.tf")
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte(`output "x" { value = 1 }`), 0o644))

	cmd := &cobra.Command{}
	cmd.Flags().String("format", "zip", "")
	cmd.Flags().Bool("oci-compliant", false, "")
	cmd.Flags().Bool("strict-oci", true, "")
	cmd.Flags().Bool("json", true, "")
	cmd.Flags().String("metadata-out", "", "")
	cmd.Flags().String("strip", string(tofupress.StripModeOptimistic), "")
	cmd.Flags().String("vendor-dir", "_vendor", "")

	var stdout bytes.Buffer
	cmd.SetOut(&stdout)

	require.NoError(t, runPressTree(cmd, []string{subject, filepath.Join(t.TempDir(), "tree.zip")}))
	assert.Contains(t, stdout.String(), `"name": "tree"`)
}

func readZipNames(t *testing.T, path string) []string {
	t.Helper()
	r, err := zip.OpenReader(path)
	require.NoError(t, err)
	defer r.Close()
	var names []string
	for _, f := range r.File {
		if !f.FileInfo().IsDir() {
			names = append(names, f.Name)
		}
	}
	return names
}
