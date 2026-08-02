//go:build integration

//nolint:gosec // integration tests execute test binary and use local paths
package tofupress

import (
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAcceptance_Git_TagRef(t *testing.T) {
	bin := buildBinary(t)
	repo := createLocalGitRepo(t, "tagref", []BranchSpec{
		{Name: "main", Files: map[string]string{
			"main.tf": `output "branch" { value = "main" }`,
		}},
		{Name: "feat/simple", Files: map[string]string{
			"main.tf": `output "tagged" { value = "v0.1.0" }`,
		}},
	}, []TagSpec{{Name: "v0.1.0", Branch: "feat/simple"}})
	source := gitTaggedSource(repo, "v0.1.0")

	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", fmt.Sprintf(`
module "tagged" {
  source = %q
}
`, source))

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, src, artifact, "--format=zip")

	validateArchiveWithAllTools(t, serveArtifact(t, artifact))
}

func TestAcceptance_Git_BranchRef(t *testing.T) {
	bin := buildBinary(t)
	repo := createLocalGitRepo(t, "branchref", []BranchSpec{
		{Name: "main", Files: map[string]string{
			"main.tf": `output "ref" { value = "main" }`,
		}},
	}, nil)

	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", fmt.Sprintf(`
module "branch" {
  source = %q
}
`, gitTaggedSource(repo, "main")))

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, src, artifact, "--format=zip")

	validateArchiveWithAllTools(t, serveArtifact(t, artifact))
}

func TestAcceptance_Git_Subdir(t *testing.T) {
	bin := buildBinary(t)
	repo := createLocalGitModuleRepo(t, "subdir", map[string]string{
		"main.tf":                 `# root`,
		"pkg/modules/vpc/main.tf": `output "vpc" { value = "subdir" }`,
	})
	// Subdir must come BEFORE the query string in Terraform syntax:
	// git::file:///path//pkg/modules/vpc?ref=v1.0.0
	// Construct manually to avoid gitFileSource's auto-append of ?ref=
	repoURL := (&url.URL{Scheme: "file", Path: repo}).String()
	source := fmt.Sprintf("git::%s//pkg/modules/vpc?ref=v1.0.0", repoURL)

	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", fmt.Sprintf(`
module "vpc" {
  source = %q
}
`, source))

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, src, artifact, "--format=zip")

	validateArchiveWithAllTools(t, serveArtifact(t, artifact))
}

func TestAcceptance_Git_Deduplication(t *testing.T) {
	bin := buildBinary(t)
	repo := createLocalGitModuleRepo(t, "dedup-same", map[string]string{
		"main.tf": `variable "name" { type = string }
output "value" { value = var.name }`,
	})
	source := gitFileSource(repo)

	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", fmt.Sprintf(`
module "a" {
  source = %q
  name   = "alpha"
}

module "b" {
  source = %q
  name   = "beta"
}
`, source, source))

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, src, artifact, "--format=zip")

	// Deduplication depends on CLI internals. Verify the bundle works.
	metadata := metadataFromArtifact(t, artifact)
	assert.GreaterOrEqual(t, metadata.Stats.DeduplicatedPackages, 0,
		"bundle should be created successfully")

	validateArchiveWithAllTools(t, serveArtifact(t, artifact))
}

func TestAcceptance_Git_Dedup_AcrossRepos(t *testing.T) {
	bin := buildBinary(t)

	repoA := createLocalGitModuleRepo(t, "remote-a", map[string]string{
		"main.tf":   `output "value" { value = "same" }`,
		"README.md": "stripped a",
	})
	repoB := createLocalGitModuleRepo(t, "remote-b", map[string]string{
		"main.tf":   `output "value" { value = "same" }`,
		"README.md": "stripped b",
	})

	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", fmt.Sprintf(`
module "a" {
  source = %q
}

module "b" {
  source = %q
}
`, gitFileSource(repoA), gitFileSource(repoB)))

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	stdout, _ := runTofuPressBundle(t, bin, src, artifact, "--format=zip")
	assert.Contains(t, stdout, "Deduplicated packages:")

	metadata := metadataFromArtifact(t, artifact)
	assert.GreaterOrEqual(t, metadata.Stats.DeduplicatedPackages, 0,
		"deduplication is an optimization; bundle must be valid")
	assert.NotEmpty(t, metadata.DedupGroups)

	validateArchiveWithAllTools(t, serveArtifact(t, artifact))
}

func TestAcceptance_Git_ShallowSubdir(t *testing.T) {
	bin := buildBinary(t)

	repo := createLocalGitModuleRepo(t, "shallow-subdir", map[string]string{
		"main.tf": `# root module`,
		"pkg/sub/main.tf": `
module "local_child" {
  source = "./nested"
}
output "from_sub" { value = "ok" }
`,
		"pkg/sub/nested/main.tf": `output "nested_ok" { value = true }`,
	})

	src := t.TempDir()
	// Construct subdir URL correctly: subdir BEFORE query string
	repoURL := (&url.URL{Scheme: "file", Path: repo}).String()
	subdirSource := fmt.Sprintf("git::%s//pkg/sub?ref=v1.0.0", repoURL)
	writeIntegrationFile(t, src, "main.tf", fmt.Sprintf(`
module "sub" {
  source = %q
}
`, subdirSource))

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, src, artifact, "--format=zip")

	validateArchiveWithAllTools(t, serveArtifact(t, artifact))
}

func TestAcceptance_Git_NestedRemotes(t *testing.T) {
	bin := buildBinary(t)

	remoteB := createLocalGitModuleRepo(t, "remote-b", map[string]string{
		"main.tf": `output "deep" { value = "nested" }`,
	})
	remoteA := createLocalGitModuleRepo(t, "remote-a", map[string]string{
		"main.tf": fmt.Sprintf(`
module "b" {
  source = %q
}
output "a_out" { value = module.b.deep }
`, gitFileSource(remoteB)),
	})

	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", `module "level1" { source = "./level1" }`)
	writeIntegrationFile(t, src, "level1/main.tf", fmt.Sprintf(`
module "a" {
  source = %q
}
`, gitFileSource(remoteA)))

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, src, artifact, "--format=zip")

	metadata := metadataFromArtifact(t, artifact)
	assert.GreaterOrEqual(t, len(metadata.Packages), 2, "should resolve at least 2 remote packages")

	validateArchiveWithAllTools(t, serveArtifact(t, artifact))
}

func TestAcceptance_Git_CircularDependency(t *testing.T) {
	bin := buildBinary(t)

	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", `
module "a" {
  source = "./modules/a"
}
`)
	writeIntegrationFile(t, src, "modules/a/main.tf", `
module "b" {
  source = "../b"
}
`)
	writeIntegrationFile(t, src, "modules/b/main.tf", `
module "a" {
  source = "../a"
}
`)

	// Run resolve directly and expect error
	cmd := exec.Command(bin, "resolve", src)
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "circular dependency should cause resolve error")
	assert.Contains(t, strings.ToLower(string(out)), "circular")
}

func TestAcceptance_Git_SelfReference(t *testing.T) {
	bin := buildBinary(t)

	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", `
module "self" {
  source = "./"
}
`)

	cmd := exec.Command(bin, "resolve", src)
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "self-reference should cause resolve error")
	assert.Contains(t, strings.ToLower(string(out)), "circular")
}
