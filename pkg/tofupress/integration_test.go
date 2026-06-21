//nolint:gosec // test files use standard permissions and safe paths
package tofupress

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cloneRepo is a test helper that clones a git repository to a temporary directory.
//
//nolint:unused // kept for future integration tests
func cloneRepo(t *testing.T, repoURL, dest string) {
	t.Helper()
	cmd := exec.Command("git", "clone", "--depth=1", repoURL, dest)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "git clone failed: %s", string(output))
}

func TestIntegration_FullResolutionWithRealRepo(t *testing.T) { //nolint:gocognit // complex integration test with multiple verification steps
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Create a test scenario that exercises the full resolution pipeline
	tmpDir := t.TempDir()

	// Root module with multiple module types
	writeTerraformFile(t, tmpDir, "main.tf", `
module "local_module" {
  source = "./modules/local"
}

module "remote_vpc" {
  source = "git::https://github.com/terraform-aws-modules/terraform-aws-vpc.git?ref=v5.0.0"
  name   = "test-vpc"
  cidr   = "10.0.0.0/16"
}

module "remote_ec2" {
  source = "git::https://github.com/terraform-aws-modules/terraform-aws-ec2-instance.git?ref=v5.0.0"
  name   = "test-instance"
}
`)

	// Create local module
	localDir := filepath.Join(tmpDir, "modules", "local")
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	writeTerraformFile(t, localDir, "main.tf", `
variable "test" {
  type = string
}
`)

	// Resolve
	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), tmpDir)
	require.NoError(t, err)
	require.NotNil(t, tree)

	// Verify tree structure
	t.Logf("Total modules: %d", len(tree.AllModules))
	t.Logf("Downloaded packages: %d", len(tree.Packages))

	// Should have root + 3 modules
	assert.Equal(t, 4, len(tree.AllModules), "expected root + 3 modules")

	// Should have 2 remote packages (vpc and ec2)
	assert.Equal(t, 2, len(tree.Packages), "expected 2 remote packages")

	// Verify all remote modules were downloaded
	for _, pkg := range tree.Packages {
		assert.DirExists(t, pkg.LocalDir, "package directory should exist: %s", pkg.PackageAddr)
	}

	// Verify all module sources were rewritten to local paths
	for _, mod := range tree.AllModules {
		if !mod.IsRemote {
			continue
		}

		parentDir := mod.Parent.InstallDir
		if parentDir == "" {
			parentDir = tmpDir
		}

		tfFiles, err := FindTerraformFiles(parentDir)
		require.NoError(t, err)

		for _, tfFile := range tfFiles {
			modules, err := ExtractModuleBlocks(tfFile)
			require.NoError(t, err)

			for _, m := range modules {
				if m.Name == mod.Name {
					assert.True(t, IsLocalSource(m.Source),
						"module %s source should be rewritten to local path, got: %s",
						mod.Key, m.Source)
				}
			}
		}
	}

	// Print tree structure for debugging
	t.Logf("\nModule Tree:")
	for _, mod := range tree.AllModules {
		t.Logf("  %s (key=%s, local=%v, remote=%v)",
			mod.Name, mod.Key, mod.IsLocal, mod.IsRemote)
		if mod.IsRemote {
			t.Logf("    -> %s", mod.InstallDir)
		}
	}
}

func TestIntegration_DeduplicationWithRealRepo(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Create a test scenario with duplicate remote modules
	tmpDir := t.TempDir()

	// Root module that references the same remote module twice
	writeTerraformFile(t, tmpDir, "main.tf", `
module "vpc1" {
  source = "git::https://github.com/terraform-aws-modules/terraform-aws-vpc.git?ref=v5.0.0"
  name   = "vpc1"
  cidr   = "10.0.0.0/16"
}

module "vpc2" {
  source = "git::https://github.com/terraform-aws-modules/terraform-aws-vpc.git?ref=v5.0.0"
  name   = "vpc2"
  cidr   = "10.1.0.0/16"
}

module "vpc3" {
  source = "git::https://github.com/terraform-aws-modules/terraform-aws-vpc.git?ref=v5.0.0"
  name   = "vpc3"
  cidr   = "10.2.0.0/16"
}
`)

	// Resolve
	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), tmpDir)
	require.NoError(t, err)
	require.NotNil(t, tree)

	// Verify deduplication
	assert.Len(t, tree.AllModules, 4, "should have root + 3 vpc modules")
	assert.Len(t, tree.Packages, 1, "should only download ONE package (deduplicated)")

	// All three modules should point to the same local directory
	vpc1 := tree.Find("vpc1")
	vpc2 := tree.Find("vpc2")
	vpc3 := tree.Find("vpc3")

	require.NotNil(t, vpc1)
	require.NotNil(t, vpc2)
	require.NotNil(t, vpc3)

	assert.Equal(t, vpc1.InstallDir, vpc2.InstallDir, "vpc1 and vpc2 should share install dir")
	assert.Equal(t, vpc2.InstallDir, vpc3.InstallDir, "vpc2 and vpc3 should share install dir")

	// Verify the directory exists
	assert.DirExists(t, vpc1.InstallDir)
}

func TestIntegration_DeepNestingWithRemoteModules(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Create a deeply nested structure
	tmpDir := t.TempDir()

	// Root -> level1 -> level2 -> remote_module
	writeTerraformFile(t, tmpDir, "main.tf", `
module "level1" {
  source = "./level1"
}
`)

	level1Dir := filepath.Join(tmpDir, "level1")
	require.NoError(t, os.MkdirAll(level1Dir, 0o755))
	writeTerraformFile(t, level1Dir, "main.tf", `
module "level2" {
  source = "./level2"
}
`)

	level2Dir := filepath.Join(level1Dir, "level2")
	require.NoError(t, os.MkdirAll(level2Dir, 0o755))
	writeTerraformFile(t, level2Dir, "main.tf", `
module "remote" {
  source = "git::https://github.com/terraform-aws-modules/terraform-aws-vpc.git?ref=v5.0.0"
  name   = "deep-vpc"
  cidr   = "10.0.0.0/16"
}
`)

	// Resolve
	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), tmpDir)
	require.NoError(t, err)
	require.NotNil(t, tree)

	// Verify tree structure
	assert.Len(t, tree.AllModules, 4, "root + level1 + level2 + remote")
	assert.Len(t, tree.Packages, 1)

	// Find the deeply nested remote module
	remote := tree.Find("level1.level2.remote")
	require.NotNil(t, remote)
	assert.True(t, remote.IsRemote)
	assert.Contains(t, remote.InstallDir, "sourcetree/")

	// Verify parent chain
	assert.Equal(t, "level2", remote.Parent.Name)
	assert.Equal(t, "level1", remote.Parent.Parent.Name)
	assert.Equal(t, "root", remote.Parent.Parent.Parent.Name)
}

func TestIntegration_MixedLocalAndRemoteModules(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Create a module with both local and remote dependencies
	tmpDir := t.TempDir()

	writeTerraformFile(t, tmpDir, "main.tf", `
module "local_vpc" {
  source = "./modules/vpc"
}

module "remote_vpc" {
  source = "git::https://github.com/terraform-aws-modules/terraform-aws-vpc.git?ref=v5.0.0"
  name   = "remote-vpc"
  cidr   = "10.0.0.0/16"
}
`)

	// Create local module
	localVpcDir := filepath.Join(tmpDir, "modules", "vpc")
	require.NoError(t, os.MkdirAll(localVpcDir, 0o755))
	writeTerraformFile(t, localVpcDir, "main.tf", `
variable "cidr" {
  type = string
}
`)

	// Resolve
	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), tmpDir)
	require.NoError(t, err)
	require.NotNil(t, tree)

	// Verify tree structure
	assert.Len(t, tree.AllModules, 3, "root + local_vpc + remote_vpc")
	assert.Len(t, tree.Packages, 1, "one remote package")

	// Find modules
	localVpc := tree.Find("local_vpc")
	remoteVpc := tree.Find("remote_vpc")

	require.NotNil(t, localVpc)
	require.NotNil(t, remoteVpc)

	// Verify types
	assert.True(t, localVpc.IsLocal)
	assert.False(t, localVpc.IsRemote)
	assert.False(t, remoteVpc.IsLocal)
	assert.True(t, remoteVpc.IsRemote)

	// Verify paths
	assert.NotContains(t, localVpc.InstallDir, "sourcetree/")
	assert.Contains(t, remoteVpc.InstallDir, "sourcetree/")
}

func TestIntegration_ResolvedTreeSerialization(t *testing.T) {
	// Test that the tree structure is correct and can be traversed
	tmpDir := t.TempDir()

	writeTerraformFile(t, tmpDir, "main.tf", `
module "a" {
  source = "./modules/a"
}

module "b" {
  source = "./modules/b"
}
`)

	modADir := filepath.Join(tmpDir, "modules", "a")
	require.NoError(t, os.MkdirAll(modADir, 0o755))
	writeTerraformFile(t, modADir, "main.tf", `
module "c" {
  source = "./c"
}
`)

	modCDir := filepath.Join(modADir, "c")
	require.NoError(t, os.MkdirAll(modCDir, 0o755))
	writeTerraformFile(t, modCDir, "main.tf", `# leaf module`)

	modBDir := filepath.Join(tmpDir, "modules", "b")
	require.NoError(t, os.MkdirAll(modBDir, 0o755))
	writeTerraformFile(t, modBDir, "main.tf", `# leaf module`)

	// Resolve
	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), tmpDir)
	require.NoError(t, err)
	require.NotNil(t, tree)

	// Verify structure
	assert.Len(t, tree.AllModules, 4)

	// Verify keys are correct
	a := tree.Find("a")
	b := tree.Find("b")
	c := tree.Find("a.c")

	require.NotNil(t, a)
	require.NotNil(t, b)
	require.NotNil(t, c)

	// Verify parent-child relationships
	assert.Equal(t, tree.Root, a.Parent)
	assert.Equal(t, tree.Root, b.Parent)
	assert.Equal(t, a, c.Parent)

	// Verify children
	assert.Len(t, tree.Root.Children, 2)
	assert.Len(t, a.Children, 1)
	assert.Len(t, b.Children, 0)
	assert.Len(t, c.Children, 0)
}

func TestIntegration_TreeStatistics(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Create a complex tree to test statistics
	tmpDir := t.TempDir()

	writeTerraformFile(t, tmpDir, "main.tf", `
module "local1" {
  source = "./modules/local1"
}

module "remote1" {
  source = "git::https://github.com/terraform-aws-modules/terraform-aws-vpc.git?ref=v5.0.0"
  name   = "remote1"
}

module "remote2" {
  source = "git::https://github.com/terraform-aws-modules/terraform-aws-vpc.git?ref=v5.0.0"
  name   = "remote2"
}
`)

	local1Dir := filepath.Join(tmpDir, "modules", "local1")
	require.NoError(t, os.MkdirAll(local1Dir, 0o755))
	writeTerraformFile(t, local1Dir, "main.tf", `# local`)

	// Resolve
	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), tmpDir)
	require.NoError(t, err)
	require.NotNil(t, tree)

	// Test statistics
	assert.Equal(t, 4, tree.ModuleCount())
	assert.Equal(t, 1, tree.PackageCount()) // deduplicated
	assert.Equal(t, 2, tree.LocalModuleCount())
	assert.Equal(t, 2, tree.RemoteModuleCount())

	// Verify Contains and Find
	assert.True(t, tree.Contains("local1"))
	assert.True(t, tree.Contains("remote1"))
	assert.True(t, tree.Contains("remote2"))
	assert.False(t, tree.Contains("nonexistent"))
}
