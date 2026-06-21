//go:build integration

//nolint:gosec // test files use temporary directories and local file URLs
package tofupress

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createLocalGitModuleRepo(t *testing.T, name string, files map[string]string) string {
	t.Helper()
	repoDir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(repoDir, 0o755))
	for rel, content := range files {
		writeIntegrationFile(t, repoDir, rel, content)
	}
	runCommand(t, repoDir, nil, "git", "init")
	runCommand(t, repoDir, nil, "git", "config", "user.email", "integration@example.invalid")
	runCommand(t, repoDir, nil, "git", "config", "user.name", "TofuPress Integration")
	runCommand(t, repoDir, nil, "git", "add", ".")
	runCommand(t, repoDir, nil, "git", "commit", "-m", "initial fixture")
	runCommand(t, repoDir, nil, "git", "tag", "v1.0.0")
	return repoDir
}

func gitFileSource(repoDir string) string {
	return "git::" + (&url.URL{Scheme: "file", Path: repoDir}).String() + "?ref=v1.0.0"
}

func TestIntegrationLocalGitResolutionAndDeduplication(t *testing.T) {
	repo := createLocalGitModuleRepo(t, "remote-module", map[string]string{
		"main.tf": `
variable "name" { type = string }
output "name" { value = var.name }
`,
	})

	rootDir := t.TempDir()
	source := gitFileSource(repo)
	writeIntegrationFile(t, rootDir, "main.tf", fmt.Sprintf(`
module "one" {
  source = %q
  name   = "one"
}

module "two" {
  source = %q
  name   = "two"
}
`, source, source))

	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), rootDir)
	require.NoError(t, err)
	require.NotNil(t, tree)

	assert.Len(t, tree.AllModules, 3, "root plus two remote module references")
	assert.Len(t, tree.Packages, 1, "same git source should be deduplicated")

	one := tree.Find("one")
	two := tree.Find("two")
	require.NotNil(t, one)
	require.NotNil(t, two)
	assert.True(t, one.IsRemote)
	assert.True(t, two.IsRemote)
	assert.Equal(t, one.InstallDir, two.InstallDir)
	assert.DirExists(t, one.InstallDir)
}

func TestIntegrationLocalNestedResolution(t *testing.T) {
	repo := createLocalGitModuleRepo(t, "leaf-module", map[string]string{
		"main.tf": `output "leaf" { value = "ok" }`,
	})

	rootDir := t.TempDir()
	writeIntegrationFile(t, rootDir, "main.tf", `
module "level1" {
  source = "./level1"
}
`)
	writeIntegrationFile(t, rootDir, "level1/main.tf", fmt.Sprintf(`
module "leaf" {
  source = %q
}
`, gitFileSource(repo)))

	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), rootDir)
	require.NoError(t, err)

	assert.Len(t, tree.AllModules, 3)
	assert.Len(t, tree.Packages, 1)
	leaf := tree.Find("level1.leaf")
	require.NotNil(t, leaf)
	assert.True(t, leaf.IsRemote)
	assert.Equal(t, "level1", leaf.Parent.Name)
}
