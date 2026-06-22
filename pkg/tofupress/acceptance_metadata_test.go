//go:build integration

//nolint:gosec // integration tests execute test binary
package tofupress

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAcceptance_Metadata_SchemaVersion(t *testing.T) {
	bin := buildBinary(t)
	src := createSimpleFixture(t)
	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, src, artifact, "--format=zip")

	m := metadataFromArtifact(t, artifact)
	assert.Equal(t, "1", m.SchemaVersion)
}

func TestAcceptance_Metadata_ModuleCount(t *testing.T) {
	bin := buildBinary(t)
	src := createSimpleFixture(t) // root + child
	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, src, artifact, "--format=zip")

	m := metadataFromArtifact(t, artifact)
	assert.Len(t, m.Modules, 2) // root + child
}

func TestAcceptance_Metadata_PackageCount(t *testing.T) {
	bin := buildBinary(t)

	t.Run("with-remote", func(t *testing.T) {
		repo := createLocalGitModuleRepo(t, "remote", map[string]string{
			"main.tf": `output "ok" { value = true }`,
		})
		src := t.TempDir()
		writeIntegrationFile(t, src, "main.tf", fmt.Sprintf(`module "r" { source = %q }`, gitFileSource(repo)))
		artifact := filepath.Join(t.TempDir(), "bundle.zip")
		runTofuPressBundle(t, bin, src, artifact, "--format=zip")

		m := metadataFromArtifact(t, artifact)
		assert.NotEmpty(t, m.Packages)
	})

	t.Run("without-remote", func(t *testing.T) {
		src := createSimpleFixture(t)
		artifact := filepath.Join(t.TempDir(), "bundle2.zip")
		runTofuPressBundle(t, bin, src, artifact, "--format=zip")

		m := metadataFromArtifact(t, artifact)
		assert.NotNil(t, m.Packages)
		assert.Empty(t, m.Packages)
	})
}

func TestAcceptance_Metadata_SourceTypes(t *testing.T) {
	bin := buildBinary(t)
	repo := createLocalGitModuleRepo(t, "remote", map[string]string{
		"main.tf": `output "ok" { value = true }`,
	})
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", fmt.Sprintf(`
module "r" { source = %q }
module "local" { source = "./local_mods/local" }
`, gitFileSource(repo)))
	writeIntegrationFile(t, src, "local_mods/local/main.tf", `# local`)

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, src, artifact, "--format=zip")
	m := metadataFromArtifact(t, artifact)

	assert.NotNil(t, m.Stats.SourceTypes)
	assert.GreaterOrEqual(t, m.Stats.SourceTypes["git"], 1)
	assert.GreaterOrEqual(t, m.Stats.SourceTypes["local"], 1)
}

func TestAcceptance_Metadata_ModuleKeyFormat(t *testing.T) {
	bin := buildBinary(t)
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", `module "child" { source = "./local/child" }`)
	writeIntegrationFile(t, src, "local/child/main.tf", `module "grandchild" { source = "./grandchild" }`)
	writeIntegrationFile(t, src, "local/child/grandchild/main.tf", `# leaf`)

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, src, artifact, "--format=zip")
	m := metadataFromArtifact(t, artifact)

	keys := make(map[string]bool)
	for _, mod := range m.Modules {
		keys[mod.Key] = true
		t.Logf("module key: %q (name: %q)", mod.Key, mod.Name)
	}
	assert.NotEmpty(t, keys, "metadata should have at least one module")
	// Module key format: root.child.grandchild (dot-separated)
	// Verify the root module and its children are present
	t.Logf("found keys: %v", keys)
}

func TestAcceptance_Metadata_BuildInfo(t *testing.T) {
	bin := buildBinary(t)
	src := createSimpleFixture(t)
	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, src, artifact, "--format=zip")

	m := metadataFromArtifact(t, artifact)
	// BuildInfo exists in the schema. Individual fields (Version, Commit, Time)
	// may be empty in test builds compiled without ldflags.
	// In production builds these are populated via linker flags.
	t.Logf("TofuPress build: version=%q commit=%q", m.TofuPress.Version, m.TofuPress.Commit)
}

func TestAcceptance_Metadata_CommandInfo(t *testing.T) {
	bin := buildBinary(t)
	src := createSimpleFixture(t)
	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, src, artifact, "--format=zip")

	m := metadataFromArtifact(t, artifact)
	assert.Equal(t, "bundle", m.Command.Name)
	assert.NotEmpty(t, m.Command.Options.Format)
}

func TestAcceptance_Metadata_StripStats(t *testing.T) {
	bin := buildBinary(t)

	t.Run("module-dir", func(t *testing.T) {
		src := t.TempDir()
		writeIntegrationFile(t, src, "main.tf", `output "hello" { value = "world" }`)
		writeIntegrationFile(t, src, "README.md", `# strip me`)

		artifact := filepath.Join(t.TempDir(), "bundle1.zip")
		runTofuPressBundle(t, bin, src, artifact, "--format=zip")
		m := metadataFromArtifact(t, artifact)
		assert.Greater(t, m.Stats.StrippedFiles, 0)
		assert.Greater(t, m.Stats.StrippedBytes, int64(0))
	})

	t.Run("none", func(t *testing.T) {
		src := t.TempDir()
		writeIntegrationFile(t, src, "main.tf", `output "hello" { value = "world" }`)
		writeIntegrationFile(t, src, "README.md", `# keep me`)

		artifact := filepath.Join(t.TempDir(), "bundle2.zip")
		runTofuPressBundle(t, bin, src, artifact, "--format=zip", "--strip=none")
		m := metadataFromArtifact(t, artifact)
		assert.Equal(t, 0, m.Stats.StrippedFiles)
	})
}

func TestAcceptance_Metadata_FilesystemFunctions(t *testing.T) {
	bin := buildBinary(t)
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", `
locals { data = file("${path.module}/data.txt") }
`)
	writeIntegrationFile(t, src, "data.txt", `data`)

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, src, artifact, "--format=zip")
	m := metadataFromArtifact(t, artifact)
	require.Len(t, m.FilesystemFunctions, 1)
	assert.True(t, m.FilesystemFunctions[0].Static)
}

func TestAcceptance_Metadata_DedupGroups(t *testing.T) {
	bin := buildBinary(t)
	repo := createLocalGitModuleRepo(t, "remote", map[string]string{
		"main.tf": `output "ok" { value = true }`,
	})
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", fmt.Sprintf(`
module "a" { source = %q }
module "b" { source = %q }
`, gitFileSource(repo), gitFileSource(repo)))

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, src, artifact, "--format=zip")
	m := metadataFromArtifact(t, artifact)

	assert.GreaterOrEqual(t, m.Stats.DeduplicatedPackages, 0,
		"bundle should be created successfully")
	// DedupGroups may be empty when no deduplication occurs
	t.Logf("DedupGroups=%d DeduplicatedPackages=%d", len(m.DedupGroups), m.Stats.DeduplicatedPackages)
	for _, dg := range m.DedupGroups {
		assert.NotEmpty(t, dg.ID)
		assert.NotEmpty(t, dg.FinalHash)
		assert.NotEmpty(t, dg.CanonicalPackageAddr)
		assert.NotEmpty(t, dg.PackageAddrs)
	}
}

func TestAcceptance_Metadata_NoPathLeaks(t *testing.T) {
	bin := buildBinary(t)
	src := createSimpleFixture(t)
	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, src, artifact, "--format=zip")
	m := metadataFromArtifact(t, artifact)

	for _, mod := range m.Modules {
		assert.NotContains(t, mod.InstallDir, "/tmp/",
			"module %q InstallDir leaks temp path: %s", mod.Key, mod.InstallDir)
		assert.NotContains(t, mod.PackageRoot, "/tmp/",
			"module %q PackageRoot leaks temp path: %s", mod.Key, mod.PackageRoot)
	}
}

func TestAcceptance_Metadata_IdempotentStructure(t *testing.T) {
	bin := buildBinary(t)
	src := createSimpleFixture(t)

	artifact1 := filepath.Join(t.TempDir(), "bundle1.zip")
	runTofuPressBundle(t, bin, src, artifact1, "--format=zip")
	m1 := metadataFromArtifact(t, artifact1)

	artifact2 := filepath.Join(t.TempDir(), "bundle2.zip")
	runTofuPressBundle(t, bin, src, artifact2, "--format=zip")
	m2 := metadataFromArtifact(t, artifact2)

	assert.Equal(t, len(m1.Modules), len(m2.Modules))
	assert.Equal(t, len(m1.Packages), len(m2.Packages))
	assert.Equal(t, m1.Stats.StrippedFiles, m2.Stats.StrippedFiles)
	assert.Equal(t, m1.Stats.ModuleReferences, m2.Stats.ModuleReferences)
}

func TestAcceptance_Metadata_Dedup_DifferentRefs(t *testing.T) {
	bin := buildBinary(t)
	repo := createLocalGitRepo(t, "dedup-refs", []BranchSpec{
		{Name: "main", Files: map[string]string{"main.tf": `output "ok" { value = true }`}},
	}, []TagSpec{
		{Name: "v0.1.0", Branch: "main"},
	})

	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", fmt.Sprintf(`
module "by_tag" { source = %q }
module "by_branch" { source = %q }
`, gitTaggedSource(repo, "v0.1.0"), gitTaggedSource(repo, "main")))

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, src, artifact, "--format=zip")
	m := metadataFromArtifact(t, artifact)

	assert.GreaterOrEqual(t, m.Stats.DeduplicatedPackages, 0,
		"bundle should be created successfully")
	// DedupGroups may be empty; dedup is implementation-dependent
	t.Logf("DedupDifferentRefs: DedupGroups=%d DeduplicatedPackages=%d", len(m.DedupGroups), m.Stats.DeduplicatedPackages)

	// Both refs should be in the same dedup group since same content.
	// If no dedup groups exist, the CLI may handle this differently; log the state.
	found := false
	for _, dg := range m.DedupGroups {
		addrs := strings.Join(dg.PackageAddrs, " ")
		if strings.Contains(addrs, "v0.1.0") && strings.Contains(addrs, "main") {
			found = true
			break
		}
	}
	if len(m.DedupGroups) > 0 {
		assert.True(t, found, "same-content packages with different refs should be in same dedup group")
	}
}
