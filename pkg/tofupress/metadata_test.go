//nolint:gosec // test files use standard permissions and safe paths
package tofupress

import (
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

func TestBuildArtifactMetadataIncludesStripPlanStatsAndFilesystemFunctions(t *testing.T) {
	rootDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "main.tf"), []byte(`locals { rendered = file("templates/userdata.tftpl") }`), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(rootDir, "templates"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "templates", "userdata.tftpl"), []byte("hello"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "README.md"), []byte("strip"), 0o644))

	tree := &ResolvedTree{Root: &ModuleNode{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}, Packages: map[string]*DownloadedPackage{}}
	tree.AllModules = []*ModuleNode{tree.Root}
	stripPlan, err := PlanStripping(tree, StripModeModuleDir)
	require.NoError(t, err)

	metadata, err := BuildArtifactMetadata(tree, &MetadataRequest{
		Build:      BuildInfo{Version: "test"},
		Command:    "bundle",
		Options:    BundleOptions{Format: "zip", StripMode: string(StripModeModuleDir)},
		OutputPath: "bundle.zip",
		StripPlan:  stripPlan,
	})
	require.NoError(t, err)

	assert.Equal(t, string(StripModeModuleDir), metadata.Command.Options.StripMode)
	assert.Equal(t, stripPlan.FinalBytes, metadata.Stats.FinalBytes)
	assert.Equal(t, stripPlan.StrippedBytes, metadata.Stats.StrippedBytes)
	assert.Equal(t, stripPlan.StrippedFiles, metadata.Stats.StrippedFiles)
	require.Len(t, metadata.FilesystemFunctions, 1)
	assert.Equal(t, "file", metadata.FilesystemFunctions[0].Function)
	assert.Equal(t, "static-include", metadata.FilesystemFunctions[0].Handling)
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
}

func TestBuildArtifactMetadataIncludesSourcetreeDedupGroups(t *testing.T) {
	root := t.TempDir()
	finalDir := filepath.Join(root, "sourcetree", "pkg-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	require.NoError(t, os.MkdirAll(finalDir, 0o755))
	writeTerraformFile(t, finalDir, "main.tf", `output "id" { value = "same" }`)

	rootModule := &ModuleNode{Key: "root", Name: "root", InstallDir: root, PackageRoot: root}
	tree := &ResolvedTree{
		Root:       rootModule,
		AllModules: []*ModuleNode{rootModule},
		Packages: map[string]*DownloadedPackage{
			"pkg-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": {
				PackageAddr:          "git::file:///repo-a",
				LocalDir:             finalDir,
				ContentHash:          "download-a",
				SourcetreeID:         "pkg-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				FinalHash:            "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				CanonicalPackageAddr: "git::file:///repo-a",
				PackageAddrs:         []string{"git::file:///repo-a", "git::file:///repo-b"},
				ModuleKeys:           []string{"root.a", "root.b"},
				Deduplicated:         true,
			},
		},
	}
	identityPlan := &SourcetreeIdentityPlan{DedupGroups: []DedupGroup{{
		ID:                   "pkg-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		FinalHash:            "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		CanonicalPackageAddr: "git::file:///repo-a",
		PackageAddrs:         []string{"git::file:///repo-a", "git::file:///repo-b"},
		ModuleKeys:           []string{"root.a", "root.b"},
	}}}

	metadata, err := BuildArtifactMetadata(tree, &MetadataRequest{
		Command:        "bundle",
		OutputPath:     filepath.Join(root, "bundle.zip"),
		Options:        BundleOptions{Format: "zip", StripMode: string(StripModeModuleDir)},
		CreatedAt:      time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC),
		SourcetreePlan: identityPlan,
	})
	require.NoError(t, err)
	require.Len(t, metadata.Packages, 1)
	assert.Equal(t, "pkg-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", metadata.Packages[0].SourcetreeID)
	assert.True(t, metadata.Packages[0].Deduplicated)
	assert.ElementsMatch(t, []string{"git::file:///repo-a", "git::file:///repo-b"}, metadata.Packages[0].PackageAddrs)
	assert.Equal(t, 1, metadata.Stats.DeduplicatedPackages)
	require.Len(t, metadata.DedupGroups, 1)
	assert.Equal(t, metadata.Packages[0].SourcetreeID, metadata.DedupGroups[0].ID)
}
