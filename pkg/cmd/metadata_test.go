//nolint:gosec // test files use standard permissions and safe paths
package cmd

import (
	"archive/zip"
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

func TestRunMetadataTarGzBundle(t *testing.T) {
	rootDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "main.tf"), []byte(`output "name" { value = "root" }`), 0o644))
	root := &tofupress.ModuleNode{Name: "root", InstallDir: rootDir, IsLocal: true}
	tree := &tofupress.ResolvedTree{
		Root:       root,
		AllModules: []*tofupress.ModuleNode{root},
		Packages:   make(map[string]*tofupress.DownloadedPackage),
	}
	archivePath := filepath.Join(t.TempDir(), "bundle.tar.gz")
	bundler := tofupress.NewBundler(tofupress.BundleFormatTarGZ)
	bundler.Metadata = &tofupress.ArtifactMetadata{SchemaVersion: tofupress.MetadataSchemaVersion, CreatedAt: "2026-06-21T12:00:00Z"}
	require.NoError(t, bundler.Bundle(tree, archivePath))

	cmd := metadataCmd
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	require.NoError(t, runMetadata(cmd, []string{archivePath}))

	assert.Contains(t, stdout.String(), "\"schema_version\": \"1\"")
	var decoded tofupress.ArtifactMetadata
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &decoded))
	assert.Equal(t, tofupress.MetadataSchemaVersion, decoded.SchemaVersion)
}

func TestRunMetadataTarXzBundle(t *testing.T) {
	rootDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "main.tf"), []byte(`output "name" { value = "root" }`), 0o644))
	root := &tofupress.ModuleNode{Name: "root", InstallDir: rootDir, IsLocal: true}
	tree := &tofupress.ResolvedTree{
		Root:       root,
		AllModules: []*tofupress.ModuleNode{root},
		Packages:   make(map[string]*tofupress.DownloadedPackage),
	}
	archivePath := filepath.Join(t.TempDir(), "bundle.tar.xz")
	bundler := tofupress.NewBundler(tofupress.BundleFormatTarXZ)
	bundler.Metadata = &tofupress.ArtifactMetadata{SchemaVersion: tofupress.MetadataSchemaVersion, CreatedAt: "2026-06-21T12:00:00Z"}
	require.NoError(t, bundler.Bundle(tree, archivePath))

	cmd := metadataCmd
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	require.NoError(t, runMetadata(cmd, []string{archivePath}))

	assert.Contains(t, stdout.String(), "\"schema_version\": \"1\"")
	var decoded tofupress.ArtifactMetadata
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &decoded))
	assert.Equal(t, tofupress.MetadataSchemaVersion, decoded.SchemaVersion)
}

func TestRunMetadataErrors(t *testing.T) {
	tests := []struct {
		name        string
		setup       func(t *testing.T) string
		wantErrCont string
	}{
		{
			name: "non-existent file",
			setup: func(t *testing.T) string {
				t.Helper()
				return filepath.Join(t.TempDir(), "does-not-exist.zip")
			},
			wantErrCont: "no such file",
		},
		{
			name: "empty file",
			setup: func(t *testing.T) string {
				t.Helper()
				path := filepath.Join(t.TempDir(), "empty.zip")
				require.NoError(t, os.WriteFile(path, []byte{}, 0o644))
				return path
			},
			wantErrCont: "not a valid zip",
		},
		{
			name: "corrupt zip file",
			setup: func(t *testing.T) string {
				t.Helper()
				path := filepath.Join(t.TempDir(), "corrupt.zip")
				require.NoError(t, os.WriteFile(path, []byte("this is not a valid zip at all"), 0o644))
				return path
			},
			wantErrCont: "not a valid zip",
		},
		{
			name: "valid zip without meta.json",
			setup: func(t *testing.T) string {
				t.Helper()
				path := filepath.Join(t.TempDir(), "no-meta.zip")
				f, err := os.Create(path)
				require.NoError(t, err)
				zw := zip.NewWriter(f)
				w, err := zw.Create("main.tf")
				require.NoError(t, err)
				_, err = w.Write([]byte(`output "x" { value = "y" }`))
				require.NoError(t, err)
				require.NoError(t, zw.Close())
				require.NoError(t, f.Close())
				return path
			},
			wantErrCont: "meta.json",
		},
		{
			name: "truncated valid zip",
			setup: func(t *testing.T) string {
				t.Helper()
				// Create a valid zip first, then truncate it
				fullPath := filepath.Join(t.TempDir(), "full.zip")
				f, err := os.Create(fullPath)
				require.NoError(t, err)
				zw := zip.NewWriter(f)
				w, err := zw.Create("meta.json")
				require.NoError(t, err)
				_, err = w.Write([]byte(`{"schema_version":"1"}`))
				require.NoError(t, err)
				require.NoError(t, zw.Close())
				require.NoError(t, f.Close())

				data, err := os.ReadFile(fullPath)
				require.NoError(t, err)
				// Truncate to half: corrupts the zip central directory
				truncPath := filepath.Join(t.TempDir(), "truncated.zip")
				require.NoError(t, os.WriteFile(truncPath, data[:len(data)/2], 0o644))
				return truncPath
			},
			wantErrCont: "zip",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.setup(t)

			cmd := metadataCmd
			var stdout bytes.Buffer
			cmd.SetOut(&stdout)
			err := runMetadata(cmd, []string{path})

			require.Error(t, err, "expected error for %s", tt.name)
			assert.Contains(t, err.Error(), tt.wantErrCont,
				"error %q should contain %q", err.Error(), tt.wantErrCont)
		})
	}
}
