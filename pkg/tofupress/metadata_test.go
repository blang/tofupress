//nolint:gosec // test files use standard permissions and safe paths
package tofupress

import (
	"context"
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
	packageDir := filepath.Join(rootDir, "modules", "abc123")
	require.NoError(t, os.MkdirAll(packageDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "main.tf"), []byte("module \"remote\" { source = \"./modules/abc123\" }"), 0o644))
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
		Command:    "module",
		Args:       []string{rootDir, "bundle.zip"},
		Options:    BundleOptions{Format: "zip", OCICompliant: false, StripMode: "full"},
		RootSource: rootDir,
		OutputPath: "bundle.zip",
		CreatedAt:  createdAt,
	})
	require.NoError(t, err)

	assert.Equal(t, MetadataSchemaVersion, metadata.SchemaVersion)
	assert.Equal(t, "2026-06-21T12:00:00Z", metadata.CreatedAt)
	assert.Equal(t, "v0.1.0", metadata.TofuPress.Version)
	assert.Equal(t, "module", metadata.Command.Name)
	assert.Equal(t, "bundle.zip", metadata.Artifact.OutputPath)
	assert.Equal(t, "zip", metadata.Artifact.Format)
	// InstallDir should be relative to root to avoid leaking temp paths
	assert.Equal(t, ".", metadata.Root.InstallDir)
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

func TestBuildArtifactMetadataRedactsCredentialsAndEphemeralPaths(t *testing.T) {
	rootDir := t.TempDir()
	packageDir := t.TempDir()
	writeTerraformFile(t, rootDir, "main.tf", `output "x" { value = true }`)
	writeTerraformFile(t, packageDir, "main.tf", `output "remote" { value = true }`)
	secretSource := "git::https://alice:" + "password123@example.com/repo.git?ref=v1&token=secret-token"

	root := &ModuleNode{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}
	remote := &ModuleNode{
		Key:         "remote",
		Name:        "remote",
		Parent:      root,
		InstallDir:  packageDir,
		PackageRoot: packageDir,
		IsRemote:    true,
		Source:      ModuleSource{Raw: secretSource, PackageAddr: secretSource, Type: SourceGit},
	}
	root.Children = []*ModuleNode{remote}
	tree := &ResolvedTree{
		Root:       root,
		AllModules: []*ModuleNode{root, remote},
		Packages: map[string]*DownloadedPackage{
			"pkg": {PackageAddr: secretSource, LocalDir: packageDir, PackageAddrs: []string{secretSource}},
		},
	}
	sourceFile := filepath.Join(rootDir, "main.tf")
	stripPlan := &StripPlan{
		FilesystemFunctions: []FilesystemFunctionRef{{
			Function:      "file",
			SourceFile:    sourceFile,
			SourceRange:   sourceFile + ":1,1-10",
			ResolvedBase:  rootDir,
			IncludedPaths: []string{sourceFile},
		}},
		Warnings: []StripWarning{{Message: "package " + secretSource + " may omit files"}},
	}

	metadata, err := BuildArtifactMetadata(tree, &MetadataRequest{
		Build:      BuildInfo{Version: "test"},
		Command:    "module",
		Args:       []string{secretSource, "bundle.zip"},
		Options:    BundleOptions{Format: "zip", StripMode: string(StripModeOptimistic)},
		OutputPath: "bundle.zip",
		StripPlan:  stripPlan,
	})
	require.NoError(t, err)

	encoded, err := json.Marshal(metadata)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "password123")
	assert.NotContains(t, string(encoded), "secret-token")
	assert.NotContains(t, string(encoded), rootDir)
	assert.NotContains(t, string(encoded), packageDir)
	assert.Contains(t, string(encoded), "example.com")
	assert.Equal(t, sourceFile, stripPlan.FilesystemFunctions[0].SourceFile, "metadata sanitization must not mutate the strip plan")
}

func TestBuildArtifactMetadataIncludesStripPlanStatsAndFilesystemFunctions(t *testing.T) {
	rootDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "main.tf"), []byte(`locals { rendered = file("templates/userdata.tftpl") }`), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(rootDir, "templates"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "templates", "userdata.tftpl"), []byte("hello"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "README.md"), []byte("strip"), 0o644))

	tree := &ResolvedTree{Root: &ModuleNode{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}, Packages: map[string]*DownloadedPackage{}}
	tree.AllModules = []*ModuleNode{tree.Root}
	stripPlan, err := PlanStripping(context.Background(), tree, StripModeOptimistic)
	require.NoError(t, err)

	metadata, err := BuildArtifactMetadata(tree, &MetadataRequest{
		Build:      BuildInfo{Version: "test"},
		Command:    "module",
		Options:    BundleOptions{Format: "zip", StripMode: string(StripModeOptimistic)},
		OutputPath: "bundle.zip",
		StripPlan:  stripPlan,
	})
	require.NoError(t, err)

	assert.Equal(t, string(StripModeOptimistic), metadata.Command.Options.StripMode)
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

func TestRelPathNoLeak(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		rootDir string
		want    string
	}{
		// Path under rootDir — should return relative path
		{"under root", "/tmp/abc/package/main.tf", "/tmp/abc/package", "main.tf"},
		{"nested under root", "/tmp/abc/package/sub/file.tf", "/tmp/abc/package", "sub/file.tf"},
		{"two-dot name under root", "/tmp/abc/package/..cache/file.tf", "/tmp/abc/package", "..cache/file.tf"},
		// Path outside rootDir — must NOT leak the absolute temp path
		// Remote packages are in different temp dirs, so rel starts with ".."
		{"outside root (different temp dir)", "/tmp/xyz/download/module", "/tmp/abc/package", "module"},
		{"empty path", "", "/tmp/abc", ""},
		{"empty rootDir", "/tmp/abc/path", "", "/tmp/abc/path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := relPathNoLeak(tt.path, tt.rootDir)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestBuildArtifactMetadata_OutputPathIsRelative(t *testing.T) {
	rootDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "main.tf"), []byte("# empty"), 0o644))

	tree := &ResolvedTree{
		Root:       &ModuleNode{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true},
		AllModules: []*ModuleNode{{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}},
		Packages:   map[string]*DownloadedPackage{},
	}

	// Pass an absolute output path — metadata should store it relative to cwd
	absPath := filepath.Join(rootDir, "bundle.zip")
	metadata, err := BuildArtifactMetadata(tree, &MetadataRequest{
		Build:      BuildInfo{Version: "test"},
		Command:    "module",
		Args:       []string{".", absPath},
		Options:    BundleOptions{Format: "zip", StripMode: string(StripModeFull)},
		OutputPath: absPath,
		CreatedAt:  time.Now(),
	})
	require.NoError(t, err)

	// Output path must not be absolute
	assert.False(t, filepath.IsAbs(metadata.Artifact.OutputPath),
		"output_path in metadata must not be an absolute path (leaks host info): got %q", metadata.Artifact.OutputPath)

	// Command args must not contain absolute paths
	for _, arg := range metadata.Command.Args {
		assert.False(t, filepath.IsAbs(arg),
			"command args in metadata must not contain absolute paths (leaks host info): got %q", arg)
	}
}

func TestFormatSourceTypes(t *testing.T) {
	tests := []struct {
		name   string
		counts map[string]int
		want   string
	}{
		{"empty", map[string]int{}, ""},
		{"single", map[string]int{"git": 22}, "git: 22"},
		{"multiple", map[string]int{"git": 22, "local": 4}, "git: 22, local: 4"},
		{"three", map[string]int{"git": 5, "local": 3, "registry": 1}, "git: 5, local: 3, registry: 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatSourceTypes(tt.counts)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestBuildArtifactMetadataIncludesSourcetreeDedupGroups(t *testing.T) {
	root := t.TempDir()
	finalDir := filepath.Join(root, "modules", "pkg-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
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
		Command:        "module",
		OutputPath:     filepath.Join(root, "bundle.zip"),
		Options:        BundleOptions{Format: "zip", StripMode: string(StripModeOptimistic)},
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
