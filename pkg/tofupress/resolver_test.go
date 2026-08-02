//nolint:gosec // test files use standard permissions and safe paths
package tofupress

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolver_SerializesProgressCallbacks(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{}, 2)
	done := make(chan struct{}, 2)
	resolver := NewResolver(WithProgress(func(*ProgressEvent) {
		entered <- struct{}{}
		<-release
	}))

	go func() {
		resolver.report(&ProgressEvent{Type: "first"})
		done <- struct{}{}
	}()
	<-entered
	go func() {
		resolver.report(&ProgressEvent{Type: "second"})
		done <- struct{}{}
	}()

	concurrent := false
	select {
	case <-entered:
		concurrent = true
	case <-time.After(100 * time.Millisecond):
	}
	release <- struct{}{}
	if !concurrent {
		<-entered
	}
	release <- struct{}{}
	<-done
	<-done

	assert.False(t, concurrent, "parallel downloads must not invoke a caller's progress callback concurrently")
}

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

	// Verify the source was rewritten to a local vendored path
	assert.Contains(t, vpc.InstallDir, defaultVendorDir+"/")

	// Verify the main.tf was rewritten
	mainContent, err := os.ReadFile(filepath.Join(tmpDir, "main.tf"))
	require.NoError(t, err)
	assert.Contains(t, string(mainContent), "source = \"./"+defaultVendorDir+"/")
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
	require.Error(t, err)
	assert.Nil(t, tree)
	assert.Contains(t, err.Error(), "unsupported module source")
	assert.Contains(t, err.Error(), "invalid::source::format")
}

func TestResolver_RejectsInvalidModuleLabel(t *testing.T) {
	rootDir := t.TempDir()
	writeTerraformFile(t, rootDir, "main.tf", `module "../../escape" { source = "./child" }`)

	resolver := NewResolver()
	_, err := resolver.Resolve(context.Background(), rootDir)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not a valid Terraform identifier")
}

func TestResolver_RejectsUnsafeVendorDirBeforeCreatingIt(t *testing.T) {
	rootDir := t.TempDir()
	writeTerraformFile(t, rootDir, "main.tf", `output "x" { value = true }`)
	escapeDir := filepath.Join(filepath.Dir(rootDir), "escaped-vendor")

	resolver := NewResolver(WithResolverVendorDir("../escaped-vendor"))
	_, err := resolver.Resolve(context.Background(), rootDir)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "vendor directory")
	assert.NoDirExists(t, escapeDir)
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
	assert.Contains(t, err.Error(), "circular")
}

func TestResolver_RejectsTerraformFileSymlinkOutsidePackage(t *testing.T) {
	rootDir := t.TempDir()
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside.tf")
	require.NoError(t, os.WriteFile(outsideFile, []byte(`output "secret" { value = true }`), 0o600))
	if err := os.Symlink(outsideFile, filepath.Join(rootDir, "main.tf")); err != nil {
		t.Skip("symlinks not supported on this system")
	}

	resolver := NewResolver(WithPackageRoot(rootDir))
	_, err := resolver.Resolve(context.Background(), rootDir)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "terraform configuration main.tf escapes package boundary")
}

func TestResolver_DetectsCycleThroughSymlinkAlias(t *testing.T) {
	rootDir := t.TempDir()
	writeTerraformFile(t, rootDir, "main.tf", `module "again" { source = "./again" }`)
	if err := os.Symlink(".", filepath.Join(rootDir, "again")); err != nil {
		t.Skip("symlinks not supported on this system")
	}

	resolver := NewResolver(WithPackageRoot(rootDir))
	_, err := resolver.Resolve(context.Background(), rootDir)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "circular dependency detected")
}

func TestResolver_RejectsLocalModuleSymlinkOutsidePackage(t *testing.T) {
	rootDir := t.TempDir()
	outsideDir := t.TempDir()
	writeTerraformFile(t, rootDir, "main.tf", `module "outside" { source = "./linked" }`)
	writeTerraformFile(t, outsideDir, "main.tf", `output "secret" { value = true }`)
	if err := os.Symlink(outsideDir, filepath.Join(rootDir, "linked")); err != nil {
		t.Skip("symlinks not supported on this system")
	}

	resolver := NewResolver(WithPackageRoot(rootDir))
	_, err := resolver.Resolve(context.Background(), rootDir)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes package boundary")
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

func TestResolver_MaxModulesAllowsExactRemoteLimit(t *testing.T) {
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	entry, err := zw.Create("main.tf")
	require.NoError(t, err)
	_, err = entry.Write([]byte(`output "x" { value = true }`))
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(archive.Bytes())
	}))
	defer server.Close()

	rootDir := t.TempDir()
	writeTerraformFile(t, rootDir, "main.tf", fmt.Sprintf(`module "remote" { source = %q }`, server.URL+"/module.zip"))

	resolver := NewResolver(WithMaxModules(2))
	tree, err := resolver.Resolve(context.Background(), rootDir)

	require.NoError(t, err)
	assert.Len(t, tree.AllModules, 2)
}

func TestResolver_MaxModulesStopsBeforeRemoteFetch(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		http.Error(w, "must not be fetched", http.StatusInternalServerError)
	}))
	defer server.Close()

	rootDir := t.TempDir()
	writeTerraformFile(t, rootDir, "main.tf", fmt.Sprintf(`module "remote" { source = %q }`, server.URL+"/module.zip"))

	resolver := NewResolver(WithMaxModules(1))
	_, err := resolver.Resolve(context.Background(), rootDir)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "module limit 1 exceeded")
	assert.Zero(t, requests, "limit must be enforced before network I/O")
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

func TestResolveTree_MaxModulesIncludesEntrySeeds(t *testing.T) {
	subject := t.TempDir()
	for _, rel := range []string{"modules/a/main.tf", "modules/b/main.tf"} {
		path := filepath.Join(subject, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		writeTerraformFile(t, filepath.Dir(path), filepath.Base(path), `output "x" { value = true }`)
	}

	resolver := NewResolver(WithPackageRoot(subject), WithMaxModules(1))
	_, err := resolver.ResolveTree(context.Background(), subject)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "module limit 1 exceeded")
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

func TestQueryRegistryAPI_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/modules/hashicorp/consul/aws/1.0.0", r.URL.Path)
		assert.Equal(t, "tofupress/dev", r.Header.Get("User-Agent"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"source": "https://github.com/hashicorp/terraform-aws-consul.git", "version": "1.0.0", "tag": "v1.0.0"}`))
	}))
	t.Cleanup(server.Close)

	origBase := testRegistryBaseURL
	testRegistryBaseURL = server.URL
	t.Cleanup(func() { testRegistryBaseURL = origBase })

	result, err := queryRegistryAPI(context.Background(), nil, "hashicorp", "consul", "aws", "1.0.0")
	require.NoError(t, err)
	assert.Equal(t, "git::https://github.com/hashicorp/terraform-aws-consul.git?ref=v1.0.0", result)
}

func TestQueryRegistryAPI_RetryOnTransient(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"source": "https://github.com/test/repo.git", "version": "1.0.0", "tag": "v1.0.0"}`))
	}))
	t.Cleanup(server.Close)

	origBase := testRegistryBaseURL
	testRegistryBaseURL = server.URL
	t.Cleanup(func() { testRegistryBaseURL = origBase })

	result, err := queryRegistryAPI(context.Background(), nil, "test", "mod", "aws", "1.0.0")
	require.NoError(t, err)
	assert.Equal(t, 3, attempts, "expected 3 attempts (2 failures + 1 success)")
	assert.Contains(t, result, "git::https://")
}

func TestQueryRegistryAPI_NonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	origBase := testRegistryBaseURL
	testRegistryBaseURL = server.URL
	t.Cleanup(func() { testRegistryBaseURL = origBase })

	_, err := queryRegistryAPI(context.Background(), nil, "missing", "module", "aws", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "registry API returned status 404")
	assert.Contains(t, err.Error(), "missing/module/aws")
}

func TestQueryRegistryAPI_InvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`not json`))
	}))
	t.Cleanup(server.Close)

	origBase := testRegistryBaseURL
	testRegistryBaseURL = server.URL
	t.Cleanup(func() { testRegistryBaseURL = origBase })

	_, err := queryRegistryAPI(context.Background(), nil, "test", "mod", "aws", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to parse registry API response")
}

func TestQueryRegistryAPI_MissingSource(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"source": "", "version": "1.0.0"}`))
	}))
	t.Cleanup(server.Close)

	origBase := testRegistryBaseURL
	testRegistryBaseURL = server.URL
	t.Cleanup(func() { testRegistryBaseURL = origBase })

	_, err := queryRegistryAPI(context.Background(), nil, "test", "mod", "aws", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not return a source URL")
}

func TestQueryRegistryAPI_ContextCancelled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	t.Cleanup(server.Close)

	origBase := testRegistryBaseURL
	testRegistryBaseURL = server.URL
	t.Cleanup(func() { testRegistryBaseURL = origBase })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	t.Cleanup(cancel)

	_, err := queryRegistryAPI(ctx, nil, "test", "mod", "aws", "")
	require.Error(t, err)
}

func TestResolver_SelfReferenceErrorMessage(t *testing.T) {
	// A module that references itself with a valid local source must be rejected.
	// Regression test for QA Finding #8: the error message should name the
	// ancestor explicitly when the root module is involved, not show an empty
	// key (ancestor module "").
	tmpDir := t.TempDir()

	writeTerraformFile(t, tmpDir, "main.tf", `
module "self" {
  source = "./"
}
`)

	resolver := NewResolver()
	_, err := resolver.Resolve(context.Background(), tmpDir)
	require.Error(t, err, "self-referencing modules must be rejected")
	assert.Contains(t, err.Error(), "circular")
	assert.Contains(t, err.Error(), `ancestor module "root"`)
}

func TestResolver_SharedPathDeduplication(t *testing.T) {
	// When two module blocks reference the same local path under different
	// names, the resolver should deduplicate by install dir and both nodes
	// share the same InstallDir.
	tmpDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, "child"), 0o755))
	writeTerraformFile(t, filepath.Join(tmpDir, "child"), "main.tf", `resource "null_resource" "x" {}`)

	writeTerraformFile(t, tmpDir, "main.tf", `
module "a" {
  source = "./child"
}
module "b" {
  source = "./child"
}
`)

	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), tmpDir)
	require.NoError(t, err)

	assert.Len(t, tree.AllModules, 3, "root + a + b")
	assert.Len(t, tree.Packages, 0)

	a := tree.Find("a")
	b := tree.Find("b")
	require.NotNil(t, a)
	require.NotNil(t, b)
	assert.Equal(t, a.InstallDir, b.InstallDir, "same path should produce same install dir")
}

func TestResolver_RejectsDuplicateModuleNamesAcrossFiles(t *testing.T) {
	tmpDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, "child"), 0o755))
	writeTerraformFile(t, filepath.Join(tmpDir, "child"), "main.tf", `output "x" { value = true }`)
	writeTerraformFile(t, tmpDir, "a.tf", `module "vpc" { source = "./child" }`)
	writeTerraformFile(t, tmpDir, "b.tf", `module "vpc" { source = "./child" }`)

	resolver := NewResolver()
	_, err := resolver.Resolve(context.Background(), tmpDir)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `duplicate module "vpc"`)
	assert.Contains(t, err.Error(), "a.tf")
	assert.Contains(t, err.Error(), "b.tf")
}

func TestResolver_DuplicateModuleNamesCreatesConflictingKeys(t *testing.T) {
	// Duplicate module names in the same file should be rejected with a clear error.
	tmpDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, "child"), 0o755))
	writeTerraformFile(t, filepath.Join(tmpDir, "child"), "main.tf", `resource "null_resource" "x" {}`)

	writeTerraformFile(t, tmpDir, "main.tf", `
module "vpc" {
  source = "./child"
}
module "vpc" {
  source = "./child"
}
`)

	resolver := NewResolver()
	_, err := resolver.Resolve(context.Background(), tmpDir)
	require.Error(t, err, "duplicate module names must be rejected")
	assert.Contains(t, err.Error(), "duplicate module")
	assert.Contains(t, err.Error(), "vpc")
}

func TestResolver_ForEachCountDoesNotInterfere(t *testing.T) {
	// Module blocks using for_each or count are valid Terraform. The scanner
	// extracts them as regular module blocks and the resolver should process them
	// without error (the for_each/count are ignored for bundle purposes).
	tmpDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, "child"), 0o755))
	writeTerraformFile(t, filepath.Join(tmpDir, "child"), "main.tf", `resource "null_resource" "x" {}`)

	writeTerraformFile(t, tmpDir, "main.tf", `
module "a" {
  source   = "./child"
  for_each = toset(["x", "y"])
}
module "b" {
  source = "./child"
  count  = 2
}
`)

	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), tmpDir)
	require.NoError(t, err)

	// Both for_each and count should not prevent module extraction.
	assert.Len(t, tree.AllModules, 3, "root + a + b")
}

// TestResolver_SameNameChildInRemotePackageDuplicatePreventsSelfReference reproduces
// the customer-reported bug where a module tree with nested local modules referencing
// the same remote package (but different subdirectories) produces a self-referencing
// module node.
//
// Layout mimics the customer report:
//
//	repo_root/                              (package root)
//	  modules/
//	    account_config/
//	      main.tf → module "accountindex" { source = "../account_index" }
//	    account_index/
//	      main.tf → module "accountindex" { source = "git::file:///fake//helper/a?ref=v1" }
//	  live/network/infratest/               (working directory / root module)
//	    main.tf →
//	      module "account_config" { source = "../../../modules/account_config" }
//	      module "outputs" { source = "git::file:///fake//helper/o?ref=v1" }
//
// Key aspects:
//   - Two modules (outputs from root, accountindex from nested chain) reference
//     the SAME remote package (fake) but different subdirectories (//helper/o and //helper/a)
//   - The nested local module chain has the same module name (accountindex) as its child
//   - This creates key pattern: account_config.accountindex → account_config.accountindex.accountindex
//   - The bug: account_config.accountindex.accountindex gets itself listed as a child
func TestResolver_SameNameChildInRemotePackageDuplicatePreventsSelfReference(t *testing.T) {
	// Create the fake remote package (simulating a downloaded git repo)
	fakePkgDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(fakePkgDir, "helper", "a"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(fakePkgDir, "helper", "o"), 0o755))
	writeTerraformFile(t, filepath.Join(fakePkgDir, "helper", "a"), "main.tf", `# account_index helper leaf module`)
	writeTerraformFile(t, filepath.Join(fakePkgDir, "helper", "o"), "main.tf", `# ssm_output helper leaf module`)

	// Initialize git repo for the fake package
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = fakePkgDir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, string(out))
	}
	git("init")
	git("config", "user.email", "test@example.invalid")
	git("config", "user.name", "TofuPress Test")
	git("add", ".")
	git("commit", "-m", "initial")
	git("tag", "v1.0.0")

	fakePkgURL := (&url.URL{Scheme: "file", Path: fakePkgDir}).String()
	// Use git::file:///path//subdir?ref=v1 convention (subdir before query string)
	fakeSourceHelperA := fmt.Sprintf("git::%s//helper/a?ref=v1.0.0", fakePkgURL)
	fakeSourceHelperO := fmt.Sprintf("git::%s//helper/o?ref=v1.0.0", fakePkgURL)

	// Build the monorepo structure
	repoRoot := t.TempDir()
	modulesDir := filepath.Join(repoRoot, "modules")
	networkDir := filepath.Join(repoRoot, "live", "network")
	rootDir := filepath.Join(networkDir, "infratest")
	require.NoError(t, os.MkdirAll(rootDir, 0o755))

	// Local module: account_config (referenced from root as ../../../modules/account_config)
	accountConfigDir := filepath.Join(modulesDir, "account_config")
	require.NoError(t, os.MkdirAll(accountConfigDir, 0o755))
	writeTerraformFile(t, accountConfigDir, "main.tf", `
module "accountindex" {
  source = "../account_index"
}
`)

	// Local module: account_index (has child with same name as itself referencing remote)
	accountIndexDir := filepath.Join(modulesDir, "account_index")
	require.NoError(t, os.MkdirAll(accountIndexDir, 0o755))
	writeTerraformFile(t, accountIndexDir, "main.tf", fmt.Sprintf(`
module "accountindex" {
  source = "%s"
}
`, fakeSourceHelperA))

	// Root module: references account_config (local) and outputs (remote, same pkg different subdir)
	writeTerraformFile(t, rootDir, "main.tf", fmt.Sprintf(`
module "account_config" {
  source = "../../../modules/account_config"
}

module "outputs" {
  source = "%s"
}
`, fakeSourceHelperO))

	// Resolve with package root set to repo root
	resolver := NewResolver()
	resolver.PackageRoot = repoRoot
	resolver.RootDir = rootDir
	tree, err := resolver.Resolve(context.Background(), rootDir)
	require.NoError(t, err)
	require.NotNil(t, tree)

	t.Logf("Resolved: %d modules, %d packages", len(tree.AllModules), len(tree.Packages))
	for _, mod := range tree.AllModules {
		t.Logf("  %s: name=%s local=%v remote=%v install=%s children=%d",
			mod.Key, mod.Name, mod.IsLocal, mod.IsRemote, mod.InstallDir, len(mod.Children))
	}

	// BUG REPRODUCTION: The module account_config.accountindex.accountindex MUST NOT
	// have itself as a child (self-reference).
	bugMod := tree.Find("account_config.accountindex.accountindex")
	require.NotNil(t, bugMod, "account_config.accountindex.accountindex module should be in the tree")

	// Check for self-referencing: no child should have the same key as the module itself.
	for _, child := range bugMod.Children {
		assert.NotEqual(t, bugMod.Key, child.Key,
			"module %s must not have itself as a child (self-referencing)", bugMod.Key)
	}

	// The module should have NO children (it's a leaf module in the remote package)
	assert.Empty(t, bugMod.Children,
		"module %s should have no children (leaf module in remote package)", bugMod.Key)

	// Verify package count: only 1 remote package (fake), both outputs and accountindex share it
	assert.Len(t, tree.Packages, 1, "expected 1 remote package (shared by outputs and accountindex)")
}
