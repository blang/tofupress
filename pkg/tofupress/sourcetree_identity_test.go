//nolint:gosec // test files use standard permissions and safe paths
package tofupress

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeTestFile creates a file at the given path, creating parent directories if needed.
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func TestSourcetreeIDFromHash(t *testing.T) {
	id, err := SourcetreeIDFromHash("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	require.NoError(t, err)
	assert.Equal(t, "pkg-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", id)
}

func TestSourcetreeIDFromHashRejectsInvalidHash(t *testing.T) {
	_, err := SourcetreeIDFromHash("not-hex")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid package hash")
}

func TestSnapshotDirectoryWithStripUsesIncludedFinalContent(t *testing.T) {
	root := t.TempDir()
	writeTerraformFile(t, root, "main.tf", `output "x" { value = "kept" }`)
	writeTestFile(t, filepath.Join(root, "README.md"), "stripped")

	rootModule := &ModuleNode{Key: "root", Name: "root", InstallDir: root, PackageRoot: root}
	tree := &ResolvedTree{
		Root:       rootModule,
		AllModules: []*ModuleNode{rootModule},
		Packages:   map[string]*DownloadedPackage{},
	}
	stripPlan, err := PlanStripping(tree, StripModeModuleDir)
	require.NoError(t, err)

	snapshot, err := SnapshotDirectoryWithStrip(root, stripPlan)
	require.NoError(t, err)
	assert.Equal(t, 1, snapshot.FileCount)
	assert.Equal(t, int64(len(`output "x" { value = "kept" }`)), snapshot.TotalBytes)

	unstripped, err := SnapshotDirectory(root)
	require.NoError(t, err)
	assert.NotEqual(t, unstripped.Hash, snapshot.Hash)
}
