package tofupress

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGenerateUniqueID_Length verifies that unique IDs are at least 16 hex characters (64 bits)
// to avoid collision risk. 32-bit hashes have collision risk at ~65K modules.
func TestGenerateUniqueID_Length(t *testing.T) {
	id := generateUniqueID("git::https://github.com/example/module.git?ref=v1.0.0")

	// Should be at least 16 hex characters (64 bits) to avoid birthday paradox collisions
	assert.GreaterOrEqual(t, len(id), 16, "unique ID should be at least 16 hex characters (64 bits)")
}

// TestGenerateUniqueID_Deterministic verifies that the same input always produces the same output.
func TestGenerateUniqueID_Deterministic(t *testing.T) {
	input := "git::https://github.com/example/module.git?ref=v1.0.0"

	id1 := generateUniqueID(input)
	id2 := generateUniqueID(input)

	assert.Equal(t, id1, id2, "same input should produce same output")
}

// TestGenerateUniqueID_Unique verifies that different inputs produce different outputs.
func TestGenerateUniqueID_Unique(t *testing.T) {
	id1 := generateUniqueID("git::https://github.com/example/module1.git?ref=v1.0.0")
	id2 := generateUniqueID("git::https://github.com/example/module2.git?ref=v1.0.0")

	assert.NotEqual(t, id1, id2, "different inputs should produce different outputs")
}

// writeTerraformFile is a test helper that writes content to a .tf file.
func writeTerraformFile(t *testing.T, dir, filename, content string) {
	t.Helper()
	err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0644)
	require.NoError(t, err)
}

func TestResolver_ResolveLocalModulesOnly(t *testing.T) {
	// Create a root module with only local modules
	tmpDir := t.TempDir()

	// Create root module
	writeTerraformFile(t, tmpDir, "main.tf", `
module "vpc" {
  source = "./modules/vpc"
}

module "eks" {
  source = "./modules/eks"
}
`)

	// Create local modules
	vpcDir := filepath.Join(tmpDir, "modules", "vpc")
	require.NoError(t, os.MkdirAll(vpcDir, 0755))
	writeTerraformFile(t, vpcDir, "main.tf", `
variable "cidr" {
  type = string
}
`)

	eksDir := filepath.Join(tmpDir, "modules", "eks")
	require.NoError(t, os.MkdirAll(eksDir, 0755))
	writeTerraformFile(t, eksDir, "main.tf", `
variable "cluster_name" {
  type = string
}
`)

	// Resolve
	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), tmpDir)
	require.NoError(t, err)
	require.NotNil(t, tree)

	// Verify tree structure
	assert.NotNil(t, tree.Root)
	assert.Equal(t, tmpDir, tree.Root.InstallDir)
	assert.Len(t, tree.AllModules, 3) // root + vpc + eks
	assert.Len(t, tree.Packages, 0)   // no remote packages

	// Find vpc module
	vpc := tree.Find("vpc")
	require.NotNil(t, vpc)
	assert.Equal(t, "./modules/vpc", vpc.Source.Raw)
	assert.Equal(t, SourceLocal, vpc.Source.Type)
	assert.True(t, vpc.IsLocal)
	assert.False(t, vpc.IsRemote)

	// Find eks module
	eks := tree.Find("eks")
	require.NotNil(t, eks)
	assert.Equal(t, "./modules/eks", eks.Source.Raw)
	assert.Equal(t, SourceLocal, eks.Source.Type)
}

func TestResolver_ResolveWithRemoteModule(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	// Create a root module with a remote module
	tmpDir := t.TempDir()

	writeTerraformFile(t, tmpDir, "main.tf", `
module "vpc" {
  source = "git::https://github.com/terraform-aws-modules/terraform-aws-vpc.git?ref=v5.0.0"

  name = "my-vpc"
  cidr = "10.0.0.0/16"
}
`)

	// Resolve
	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), tmpDir)
	require.NoError(t, err)
	require.NotNil(t, tree)

	// Verify tree structure
	assert.Len(t, tree.AllModules, 2) // root + vpc
	assert.Len(t, tree.Packages, 1)   // one remote package

	// Find vpc module
	vpc := tree.Find("vpc")
	require.NotNil(t, vpc)
	assert.Equal(t, SourceGit, vpc.Source.Type)
	assert.True(t, vpc.IsRemote)
	assert.False(t, vpc.IsLocal)

	// Verify the source was rewritten to local path
	assert.Contains(t, vpc.InstallDir, "sourcetree/")

	// Verify the main.tf was rewritten
	mainContent, err := os.ReadFile(filepath.Join(tmpDir, "main.tf"))
	require.NoError(t, err)
	assert.Contains(t, string(mainContent), "source = \"./sourcetree/")
}

func TestResolver_ResolveDeduplication(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	// Create a root module that references the same remote module twice
	tmpDir := t.TempDir()

	writeTerraformFile(t, tmpDir, "main.tf", `
module "vpc1" {
  source = "git::https://github.com/terraform-aws-modules/terraform-aws-vpc.git?ref=v5.0.0"
  name   = "vpc1"
}

module "vpc2" {
  source = "git::https://github.com/terraform-aws-modules/terraform-aws-vpc.git?ref=v5.0.0"
  name   = "vpc2"
}
`)

	// Resolve
	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), tmpDir)
	require.NoError(t, err)
	require.NotNil(t, tree)

	// Verify deduplication - only one package downloaded
	assert.Len(t, tree.AllModules, 3) // root + vpc1 + vpc2
	assert.Len(t, tree.Packages, 1)   // only ONE remote package (deduplicated)

	// Both modules should point to the same local directory
	vpc1 := tree.Find("vpc1")
	vpc2 := tree.Find("vpc2")
	require.NotNil(t, vpc1)
	require.NotNil(t, vpc2)
	assert.Equal(t, vpc1.InstallDir, vpc2.InstallDir)
}

func TestResolver_ResolveNestedModules(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	// Create a root module with nested structure
	tmpDir := t.TempDir()

	writeTerraformFile(t, tmpDir, "main.tf", `
module "infra" {
  source = "./modules/infra"
}
`)

	// Create infra module that references a remote module
	infraDir := filepath.Join(tmpDir, "modules", "infra")
	require.NoError(t, os.MkdirAll(infraDir, 0755))
	writeTerraformFile(t, infraDir, "main.tf", `
module "vpc" {
  source = "git::https://github.com/terraform-aws-modules/terraform-aws-vpc.git?ref=v5.0.0"
  name   = "infra-vpc"
}
`)

	// Resolve
	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), tmpDir)
	require.NoError(t, err)
	require.NotNil(t, tree)

	// Verify tree structure: root -> infra -> vpc
	assert.Len(t, tree.AllModules, 3)
	assert.Len(t, tree.Packages, 1)

	// Find modules
	infra := tree.Find("infra")
	require.NotNil(t, infra)
	assert.Equal(t, SourceLocal, infra.Source.Type)

	vpc := tree.Find("infra.vpc")
	require.NotNil(t, vpc)
	assert.Equal(t, SourceGit, vpc.Source.Type)
	assert.Equal(t, infra, vpc.Parent)
}

func TestResolver_ResolveEmptyModule(t *testing.T) {
	// Create an empty module (no .tf files)
	tmpDir := t.TempDir()

	// Resolve
	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), tmpDir)
	require.NoError(t, err)
	require.NotNil(t, tree)

	// Should have just the root module
	assert.Len(t, tree.AllModules, 1)
	assert.Len(t, tree.Packages, 0)
	assert.NotNil(t, tree.Root)
}

func TestResolver_ResolveMultipleFilesInRoot(t *testing.T) {
	// Create a root module with multiple .tf files
	tmpDir := t.TempDir()

	writeTerraformFile(t, tmpDir, "main.tf", `
module "vpc" {
  source = "./modules/vpc"
}
`)

	writeTerraformFile(t, tmpDir, "outputs.tf", `
module "eks" {
  source = "./modules/eks"
}
`)

	// Create local modules
	vpcDir := filepath.Join(tmpDir, "modules", "vpc")
	require.NoError(t, os.MkdirAll(vpcDir, 0755))
	writeTerraformFile(t, vpcDir, "main.tf", `# vpc`)

	eksDir := filepath.Join(tmpDir, "modules", "eks")
	require.NoError(t, os.MkdirAll(eksDir, 0755))
	writeTerraformFile(t, eksDir, "main.tf", `# eks`)

	// Resolve
	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), tmpDir)
	require.NoError(t, err)
	require.NotNil(t, tree)

	// Should find modules from both files
	assert.Len(t, tree.AllModules, 3) // root + vpc + eks
}

func TestResolver_ResolveInvalidSource(t *testing.T) {
	// Create a module with an invalid source
	tmpDir := t.TempDir()

	writeTerraformFile(t, tmpDir, "main.tf", `
module "invalid" {
  source = "invalid::source::format"
}
`)

	// Resolve should fail
	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), tmpDir)
	assert.Error(t, err)
	assert.Nil(t, tree)
}

func TestNewResolver(t *testing.T) {
	resolver := NewResolver()
	assert.NotNil(t, resolver)
	assert.NotNil(t, resolver.fetcher)
}
