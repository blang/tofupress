//nolint:gosec // tests use explicit fixture permissions and test-owned paths
package tofupress

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadMetadataFromZipArtifact(t *testing.T) {
	rootDir := t.TempDir()
	writeTerraformFile(t, rootDir, "main.tf", `output "name" { value = "root" }`)
	root := &ModuleNode{Name: "root", InstallDir: rootDir, IsLocal: true}
	tree := &ResolvedTree{
		Root:       root,
		AllModules: []*ModuleNode{root},
		Packages:   make(map[string]*DownloadedPackage),
	}
	archivePath := filepath.Join(t.TempDir(), "bundle.zip")
	bundler := NewBundler(BundleFormatZIP)
	bundler.Metadata = &ArtifactMetadata{SchemaVersion: MetadataSchemaVersion, CreatedAt: "2026-06-21T12:00:00Z"}
	require.NoError(t, bundler.Bundle(context.Background(), tree, archivePath))

	metadata, err := ReadMetadataFromArtifact(archivePath)
	require.NoError(t, err)
	assert.Equal(t, MetadataSchemaVersion, metadata.SchemaVersion)
}

func TestReadMetadataFromDirDoesNotMaskRelocatedMetadataErrors(t *testing.T) {
	dir := t.TempDir()
	relocated := filepath.Join(dir, MetadataDir, MetadataFileName)
	require.NoError(t, os.MkdirAll(relocated, 0o755)) // a directory where the metadata file must be
	require.NoError(t, os.WriteFile(filepath.Join(dir, MetadataFileName), []byte(`{"schema_version":"legacy"}`), 0o644))

	_, err := ReadMetadataFromDir(dir)

	require.Error(t, err)
	assert.Contains(t, err.Error(), MetadataRelPath)
	assert.NotContains(t, err.Error(), "legacy")
}

func TestReadMetadataFromArtifactErrorsWhenMissing(t *testing.T) {
	rootDir := t.TempDir()
	writeTerraformFile(t, rootDir, "main.tf", `output "name" { value = "root" }`)
	root := &ModuleNode{Name: "root", InstallDir: rootDir, IsLocal: true}
	tree := &ResolvedTree{
		Root:       root,
		AllModules: []*ModuleNode{root},
		Packages:   make(map[string]*DownloadedPackage),
	}
	archivePath := filepath.Join(t.TempDir(), "bundle.zip")
	require.NoError(t, NewBundler(BundleFormatZIP).Bundle(context.Background(), tree, archivePath))

	_, err := ReadMetadataFromArtifact(archivePath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "metadata file .tofupress/meta.json not found")
}
