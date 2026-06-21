package tofupress

import (
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
	require.NoError(t, bundler.Bundle(tree, archivePath))

	metadata, err := ReadMetadataFromArtifact(archivePath)
	require.NoError(t, err)
	assert.Equal(t, MetadataSchemaVersion, metadata.SchemaVersion)
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
	require.NoError(t, NewBundler(BundleFormatZIP).Bundle(tree, archivePath))

	_, err := ReadMetadataFromArtifact(archivePath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "metadata file meta.json not found")
}
