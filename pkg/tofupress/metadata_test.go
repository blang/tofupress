//nolint:gosec // test files use standard permissions and safe paths
package tofupress

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildArtifactMetadataFromResolvedTree(t *testing.T) {
	rootDir := t.TempDir()
	packageDir := filepath.Join(rootDir, "sourcetree", "abc123")
	require.NoError(t, os.MkdirAll(packageDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "main.tf"), []byte("module \"remote\" { source = \"./sourcetree/abc123\" }"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(packageDir, "main.tf"), []byte("output \"id\" { value = \"remote\" }"), 0o644))

	root := &ModuleNode{
		Key:         "",
		Name:        "root",
		InstallDir:  rootDir,
		PackageRoot: rootDir,
		IsLocal:     true,
	}
	remote := &ModuleNode{
		Parent:      root,
		Key:         "remote",
		Name:        "remote",
		Source:      ModuleSource{Raw: "git::https://example.com/repo.git", PackageAddr: "git::https://example.com/repo.git", Type: SourceGit},
		InstallDir:  packageDir,
		PackageRoot: packageDir,
		IsRemote:    true,
	}
	root.Children = []*ModuleNode{remote}
	tree := &ResolvedTree{
		Root:       root,
		AllModules: []*ModuleNode{root, remote},
		Packages: map[string]*DownloadedPackage{
			"abc123": {
				PackageAddr: "git::https://example.com/repo.git",
				LocalDir:    packageDir,
				ContentHash: "download-hash",
			},
		},
	}

	createdAt := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	metadata, err := BuildArtifactMetadata(tree, &MetadataRequest{
		Build:      BuildInfo{Version: "v0.1.0", Commit: "abc", Time: "2026-06-21_12:00:00"},
		Command:    "bundle",
		Args:       []string{rootDir, "bundle.zip"},
		Options:    BundleOptions{Format: "zip", OCICompliant: false, StripMode: "none"},
		RootSource: rootDir,
		OutputPath: "bundle.zip",
		CreatedAt:  createdAt,
	})
	require.NoError(t, err)

	assert.Equal(t, MetadataSchemaVersion, metadata.SchemaVersion)
	assert.Equal(t, "2026-06-21T12:00:00Z", metadata.CreatedAt)
	assert.Equal(t, "v0.1.0", metadata.TofuPress.Version)
	assert.Equal(t, "bundle", metadata.Command.Name)
	assert.Equal(t, "bundle.zip", metadata.Artifact.OutputPath)
	assert.Equal(t, "zip", metadata.Artifact.Format)
	assert.Equal(t, rootDir, metadata.Root.InstallDir)
	assert.Len(t, metadata.Modules, 2)
	assert.Len(t, metadata.Packages, 1)
	assert.Equal(t, "git::https://example.com/repo.git", metadata.Packages[0].PackageAddr)
	assert.Equal(t, "download-hash", metadata.Packages[0].DownloadedHash)
	assert.NotEmpty(t, metadata.Packages[0].FinalHash)
	assert.Equal(t, 2, metadata.Stats.ModuleReferences)
	assert.Equal(t, 1, metadata.Stats.UniquePackages)
	assert.Equal(t, 1, metadata.Stats.SourceTypes["git"])
	assert.Equal(t, int64(0), metadata.Stats.StrippedBytes)
}

func TestWriteMetadataFileWritesIndentedJSON(t *testing.T) {
	metadata := &ArtifactMetadata{
		SchemaVersion: MetadataSchemaVersion,
		CreatedAt:     "2026-06-21T12:00:00Z",
		TofuPress:     BuildInfo{Version: "v0.1.0"},
	}
	outPath := filepath.Join(t.TempDir(), "meta.json")

	require.NoError(t, WriteMetadataFile(outPath, metadata))

	data, err := os.ReadFile(outPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), "\n  \"schema_version\":")

	var decoded ArtifactMetadata
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, MetadataSchemaVersion, decoded.SchemaVersion)
}
