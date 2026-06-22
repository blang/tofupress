//nolint:gosec // test files use standard permissions and safe paths
package tofupress

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0o644)
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
	require.NoError(t, os.MkdirAll(vpcDir, 0o755))
	writeTerraformFile(t, vpcDir, "main.tf", `
variable "cidr" {
  type = string
}
`)

	eksDir := filepath.Join(tmpDir, "modules", "eks")
	require.NoError(t, os.MkdirAll(eksDir, 0o755))
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
	require.NoError(t, os.MkdirAll(infraDir, 0o755))
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

func TestResolver_LocalPackageSubdirAllowsSiblingReferences(t *testing.T) {
	rootDir := t.TempDir()

	packageDir := filepath.Join(rootDir, "package")
	moduleADir := filepath.Join(packageDir, "modules", "moduleA")
	moduleBDir := filepath.Join(packageDir, "modules", "moduleB")
	require.NoError(t, os.MkdirAll(moduleADir, 0o755))
	require.NoError(t, os.MkdirAll(moduleBDir, 0o755))

	writeTerraformFile(t, rootDir, "main.tf", `
module "moduleA" {
  source = "./package//modules/moduleA"
}
`)
	writeTerraformFile(t, moduleADir, "main.tf", `
module "sibling" {
  source = "../moduleB"
}
`)
	writeTerraformFile(t, moduleBDir, "main.tf", `
output "name" {
  value = "moduleB"
}
`)

	resolver := NewResolver()
	resolver.PackageRoot = rootDir
	tree, err := resolver.Resolve(context.Background(), rootDir)
	require.NoError(t, err)

	moduleA := tree.Find("moduleA")
	require.NotNil(t, moduleA)
	assert.Equal(t, packageDir, moduleA.PackageRoot)
	assert.Equal(t, moduleADir, moduleA.InstallDir)

	sibling := tree.Find("moduleA.sibling")
	require.NotNil(t, sibling)
	assert.Equal(t, packageDir, sibling.PackageRoot)
	assert.Equal(t, moduleBDir, sibling.InstallDir)
}

func TestResolver_RejectsAbsoluteModuleSourceByDefault(t *testing.T) {
	tmpDir := t.TempDir()
	absoluteModuleDir := filepath.Join(t.TempDir(), "absolute-module")
	require.NoError(t, os.MkdirAll(absoluteModuleDir, 0o755))
	writeTerraformFile(t, absoluteModuleDir, "main.tf", `output "name" { value = "absolute" }`)

	writeTerraformFile(t, tmpDir, "main.tf", fmt.Sprintf(`
module "absolute" {
  source = %q
}
`, absoluteModuleDir))

	resolver := NewResolver()
	_, err := resolver.Resolve(context.Background(), tmpDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absolute module source paths are not portable")
	assert.Contains(t, err.Error(), absoluteModuleDir)
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
	require.NoError(t, os.MkdirAll(vpcDir, 0o755))
	writeTerraformFile(t, vpcDir, "main.tf", `# vpc`)

	eksDir := filepath.Join(tmpDir, "modules", "eks")
	require.NoError(t, os.MkdirAll(eksDir, 0o755))
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

func TestResolver_ContentHashIsComputed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	// Create a root module with a remote module
	tmpDir := t.TempDir()

	writeTerraformFile(t, tmpDir, "main.tf", `
module "vpc" {
  source = "git::https://github.com/terraform-aws-modules/terraform-aws-vpc.git?ref=v5.0.0"
  name   = "test-vpc"
}
`)

	// Resolve
	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), tmpDir)
	require.NoError(t, err)
	require.NotNil(t, tree)

	// Verify ContentHash is computed for the downloaded package
	assert.Len(t, tree.Packages, 1)
	for _, pkg := range tree.Packages {
		assert.NotEmpty(t, pkg.ContentHash, "ContentHash should be computed for downloaded modules")
		assert.Len(t, pkg.ContentHash, 64, "ContentHash should be 64-character SHA-256 hex string")
	}
}

func TestResolver_ContentBasedDeduplication(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	// This test verifies that modules with identical content from different URLs
	// are deduplicated based on content hash, not just URL.
	// We'll use two different git refs that point to the same commit.
	tmpDir := t.TempDir()

	writeTerraformFile(t, tmpDir, "main.tf", `
module "vpc_by_tag" {
  source = "git::https://github.com/terraform-aws-modules/terraform-aws-vpc.git?ref=v5.0.0"
  name   = "vpc-by-tag"
}

module "vpc_by_sha" {
  source = "git::https://github.com/terraform-aws-modules/terraform-aws-vpc.git?ref=26c38a66f12e7c6c93b6a2ba127ad68981a48671"
  name   = "vpc-by-sha"
}
`)

	// Resolve
	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), tmpDir)
	require.NoError(t, err)
	require.NotNil(t, tree)

	// Both modules should be resolved
	vpcByTag := tree.Find("vpc_by_tag")
	vpcBySha := tree.Find("vpc_by_sha")
	require.NotNil(t, vpcByTag)
	require.NotNil(t, vpcBySha)

	// With content-based deduplication, both should point to the same local directory
	// because they have identical content (same git commit)
	assert.Equal(t, vpcByTag.InstallDir, vpcBySha.InstallDir,
		"Modules with identical content should share the same local directory")

	// Should only have 1 unique package (deduplicated by content hash)
	// Note: Currently this will be 2 because content-based dedup is not implemented yet
	// This test will fail in RED phase, then pass after GREEN implementation
	assert.Len(t, tree.Packages, 1,
		"Identical content from different refs should be deduplicated into 1 package")
}

func TestResolver_CycleDetection(t *testing.T) {
	// Create a module tree with a cycle: A -> B -> A, all within the same package
	tmpDir := t.TempDir()

	writeTerraformFile(t, tmpDir, "main.tf", `
module "a" {
  source = "./a"
}
`)

	// Module A references module B
	aDir := filepath.Join(tmpDir, "a")
	require.NoError(t, os.MkdirAll(aDir, 0o755))
	writeTerraformFile(t, aDir, "main.tf", `
module "b" {
  source = "../b"
}
`)

	// Module B references back to module A (cycle!)
	bDir := filepath.Join(tmpDir, "b")
	require.NoError(t, os.MkdirAll(bDir, 0o755))
	writeTerraformFile(t, bDir, "main.tf", `
module "a_back" {
  source = "../a"
}
`)

	resolver := NewResolver()
	resolver.PackageRoot = tmpDir // Allow cross-sibling references within the same package
	_, err := resolver.Resolve(context.Background(), tmpDir)
	require.Error(t, err, "cycle detection should reject circular module references")
	assert.Contains(t, err.Error(), "cycle")
}

func TestResolver_MaxDepth(t *testing.T) {
	// Create a deep chain of nested modules, all within the same package
	tmpDir := t.TempDir()

	writeTerraformFile(t, tmpDir, "main.tf", `
module "l1" {
  source = "./l1"
}
`)

	// Build a nested chain: l1 -> l2 -> l3 -> l4 -> l5 -> l6 -> l7
	// Each level contains a module reference to ./next_level
	currentDir := tmpDir
	for i := 1; i <= 6; i++ {
		thisName := fmt.Sprintf("l%d", i)
		nextName := fmt.Sprintf("l%d", i+1)
		thisDir := filepath.Join(currentDir, thisName)
		require.NoError(t, os.MkdirAll(thisDir, 0o755))

		if i < 6 {
			writeTerraformFile(t, thisDir, "main.tf", fmt.Sprintf(`
module "%s" {
  source = "./%s"
}
`, nextName, nextName))
		} else {
			writeTerraformFile(t, thisDir, "main.tf", `# leaf`)
		}
		// Create the next level directory
		nextDir := filepath.Join(thisDir, nextName)
		require.NoError(t, os.MkdirAll(nextDir, 0o755))
		writeTerraformFile(t, nextDir, "main.tf", `# leaf`)
		currentDir = thisDir
	}

	resolver := NewResolver()
	resolver.PackageRoot = tmpDir
	resolver.MaxDepth = 3
	_, err := resolver.Resolve(context.Background(), tmpDir)
	require.Error(t, err, "should reject modules beyond max depth")
	assert.Contains(t, err.Error(), "depth")
}

func TestResolver_MaxModules(t *testing.T) {
	// Create many sibling modules
	tmpDir := t.TempDir()

	// Write root with 10 local modules
	var rootTF strings.Builder
	for i := range 10 {
		fmt.Fprintf(&rootTF, "\nmodule \"mod%d\" {\n  source = \"./modules/mod%d\"\n}\n", i, i)
	}
	writeTerraformFile(t, tmpDir, "main.tf", rootTF.String())

	// Create each module
	for i := range 10 {
		modDir := filepath.Join(tmpDir, "modules", fmt.Sprintf("mod%d", i))
		require.NoError(t, os.MkdirAll(modDir, 0o755))
		writeTerraformFile(t, modDir, "main.tf", `# empty`)
	}

	resolver := NewResolver()
	resolver.MaxModules = 5
	_, err := resolver.Resolve(context.Background(), tmpDir)
	require.Error(t, err, "should reject when module count exceeds limit")
	assert.Contains(t, err.Error(), "module")
}

func TestResolver_NoCycle_SharedModule(t *testing.T) {
	// Verify that shared modules (same module referenced from two parents, but at
	// different install paths) are NOT flagged as cycles.
	tmpDir := t.TempDir()

	writeTerraformFile(t, tmpDir, "main.tf", `
module "a" {
  source = "./modules/a"
}

module "b" {
  source = "./modules/b"
}
`)

	// Module A references a nested module in its own tree
	aDir := filepath.Join(tmpDir, "modules", "a")
	require.NoError(t, os.MkdirAll(aDir, 0o755))
	writeTerraformFile(t, aDir, "main.tf", `
module "nested" {
  source = "./nested"
}
`)
	aNestedDir := filepath.Join(aDir, "nested")
	require.NoError(t, os.MkdirAll(aNestedDir, 0o755))
	writeTerraformFile(t, aNestedDir, "main.tf", `# leaf`)

	// Module B also references a nested module (different path, no cycle)
	bDir := filepath.Join(tmpDir, "modules", "b")
	require.NoError(t, os.MkdirAll(bDir, 0o755))
	writeTerraformFile(t, bDir, "main.tf", `
module "nested" {
  source = "./nested"
}
`)
	bNestedDir := filepath.Join(bDir, "nested")
	require.NoError(t, os.MkdirAll(bNestedDir, 0o755))
	writeTerraformFile(t, bNestedDir, "main.tf", `# leaf`)

	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), tmpDir)
	require.NoError(t, err)
	require.NotNil(t, tree)

	// Should have root + a + a.nested + b + b.nested = 5 modules
	assert.Len(t, tree.AllModules, 5)
}

func TestResolve_ContextCancellation(t *testing.T) {
	rootDir := t.TempDir()
	writeTerraformFile(t, rootDir, "main.tf", `
module "child" {
  source = "./child"
}
`)
	childDir := filepath.Join(rootDir, "child")
	require.NoError(t, os.MkdirAll(childDir, 0o755))
	writeTerraformFile(t, childDir, "main.tf", `variable "name" { type = string }`)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	resolver := NewResolver()
	_, err := resolver.Resolve(ctx, rootDir)
	require.Error(t, err, "expected error for cancelled context")
	// The resolver should return context.Canceled when given a cancelled context
	assert.ErrorIs(t, err, context.Canceled,
		"expected context.Canceled, got %v", err)
}

func TestResolve_MissingSourceAttribute(t *testing.T) {
	rootDir := t.TempDir()
	writeTerraformFile(t, rootDir, "main.tf", `
module "broken" {
  # source attribute intentionally omitted
}
`)

	resolver := NewResolver()
	_, err := resolver.Resolve(context.Background(), rootDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing the required 'source' attribute")
	assert.Contains(t, err.Error(), "broken")
	assert.Contains(t, err.Error(), "main.tf")
	assert.NotContains(t, err.Error(), "cycle detected",
		"error should not mention cycles for a missing source attribute")
}
