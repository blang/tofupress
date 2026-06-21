//nolint:gosec // test files use standard permissions and safe paths
package cmd

import (
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
