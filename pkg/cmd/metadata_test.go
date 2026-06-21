//nolint:gosec // test files use standard permissions and safe paths
package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/blang/tofupress/pkg/tofupress"
)

func TestRunMetadataPrintsArtifactMetadata(t *testing.T) {
	rootDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "main.tf"), []byte(`output "name" { value = "root" }`), 0o644))
	root := &tofupress.ModuleNode{Name: "root", InstallDir: rootDir, IsLocal: true}
	tree := &tofupress.ResolvedTree{
		Root:       root,
		AllModules: []*tofupress.ModuleNode{root},
		Packages:   make(map[string]*tofupress.DownloadedPackage),
	}
	archivePath := filepath.Join(t.TempDir(), "bundle.zip")
	bundler := tofupress.NewBundler(tofupress.BundleFormatZIP)
	bundler.Metadata = &tofupress.ArtifactMetadata{SchemaVersion: tofupress.MetadataSchemaVersion, CreatedAt: "2026-06-21T12:00:00Z"}
	require.NoError(t, bundler.Bundle(tree, archivePath))

	cmd := metadataCmd
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	require.NoError(t, runMetadata(cmd, []string{archivePath}))

	assert.Contains(t, stdout.String(), "\n  \"schema_version\": \"1\"")
	var decoded tofupress.ArtifactMetadata
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &decoded))
	assert.Equal(t, tofupress.MetadataSchemaVersion, decoded.SchemaVersion)
}
