//nolint:gosec // test files use standard permissions and safe paths
package tofupress

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRewriteModuleSource(t *testing.T) {
	tmpDir := t.TempDir()
	tfFile := filepath.Join(tmpDir, "main.tf")

	content := `
module "vpc" {
  source = "terraform-aws-modules/vpc/aws"
  version = "3.0.0"
}

module "eks" {
  source = "terraform-aws-modules/eks/aws"
  cluster_name = "my-cluster"
}
`
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0o644))

	// Rewrite vpc module source
	err := RewriteModuleSource(tfFile, "vpc", "./vendor/vpc")
	require.NoError(t, err)

	// Read back and verify
	modules, err := ExtractModuleBlocks(tfFile)
	require.NoError(t, err)
	require.Len(t, modules, 2)

	// vpc should be rewritten
	assert.Equal(t, "vpc", modules[0].Name)
	assert.Equal(t, "./vendor/vpc", modules[0].Source)

	// eks should be unchanged
	assert.Equal(t, "eks", modules[1].Name)
	assert.Equal(t, "terraform-aws-modules/eks/aws", modules[1].Source)
}

func TestRewriteModuleSource_PreservesFormatting(t *testing.T) {
	tmpDir := t.TempDir()
	tfFile := filepath.Join(tmpDir, "main.tf")

	content := `# This is a comment
module "vpc" {
  source  = "terraform-aws-modules/vpc/aws"  # inline comment
  version = "3.0.0"

  # Another comment
  name = "my-vpc"
  cidr = "10.0.0.0/16"
}
`
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0o644))

	err := RewriteModuleSource(tfFile, "vpc", "./vendor/vpc")
	require.NoError(t, err)

	// Read back the file
	data, err := os.ReadFile(tfFile)
	require.NoError(t, err)

	result := string(data)

	// Check that comments are preserved
	assert.Contains(t, result, "# This is a comment")
	assert.Contains(t, result, "# Another comment")

	// Check that source was rewritten
	assert.Contains(t, result, `source  = "./vendor/vpc"`)

	// Check that other attributes are preserved
	assert.Contains(t, result, `version = "3.0.0"`)
	assert.Contains(t, result, `name = "my-vpc"`)
	assert.Contains(t, result, `cidr = "10.0.0.0/16"`)
}

func TestRewriteModuleSource_ModuleNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	tfFile := filepath.Join(tmpDir, "main.tf")

	content := `
module "vpc" {
  source = "terraform-aws-modules/vpc/aws"
}
`
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0o644))

	err := RewriteModuleSource(tfFile, "nonexistent", "./vendor/something")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "module \"nonexistent\" not found")
}

func TestRewriteModuleSource_NoModules(t *testing.T) {
	tmpDir := t.TempDir()
	tfFile := filepath.Join(tmpDir, "main.tf")

	content := `
resource "aws_instance" "example" {
  ami = "ami-12345678"
}
`
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0o644))

	err := RewriteModuleSource(tfFile, "vpc", "./vendor/vpc")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "module \"vpc\" not found")
}

func TestRewriteModuleSource_EmptyFile(t *testing.T) {
	tmpDir := t.TempDir()
	tfFile := filepath.Join(tmpDir, "empty.tf")

	require.NoError(t, os.WriteFile(tfFile, []byte(""), 0o644))

	err := RewriteModuleSource(tfFile, "vpc", "./vendor/vpc")
	assert.Error(t, err)
}

func TestRewriteModuleSource_MultipleRewrites(t *testing.T) {
	tmpDir := t.TempDir()
	tfFile := filepath.Join(tmpDir, "main.tf")

	content := `
module "vpc" {
  source = "terraform-aws-modules/vpc/aws"
}

module "eks" {
  source = "terraform-aws-modules/eks/aws"
}

module "rds" {
  source = "terraform-aws-modules/rds/aws"
}
`
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0o644))

	// Rewrite all three modules
	require.NoError(t, RewriteModuleSource(tfFile, "vpc", "./vendor/vpc"))
	require.NoError(t, RewriteModuleSource(tfFile, "eks", "./vendor/eks"))
	require.NoError(t, RewriteModuleSource(tfFile, "rds", "./vendor/rds"))

	// Verify all were rewritten
	modules, err := ExtractModuleBlocks(tfFile)
	require.NoError(t, err)
	require.Len(t, modules, 3)

	assert.Equal(t, "./vendor/vpc", modules[0].Source)
	assert.Equal(t, "./vendor/eks", modules[1].Source)
	assert.Equal(t, "./vendor/rds", modules[2].Source)
}

func TestRewriteModuleSource_LocalToRemote(t *testing.T) {
	tmpDir := t.TempDir()
	tfFile := filepath.Join(tmpDir, "main.tf")

	content := `
module "local" {
  source = "./modules/local"
}
`
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0o644))

	// Rewrite local to remote path
	err := RewriteModuleSource(tfFile, "local", "./modules/abc123/modules/local")
	require.NoError(t, err)

	modules, err := ExtractModuleBlocks(tfFile)
	require.NoError(t, err)
	require.Len(t, modules, 1)

	assert.Equal(t, "./modules/abc123/modules/local", modules[0].Source)
}

func TestRewriteModuleSource_WithComplexPath(t *testing.T) {
	tmpDir := t.TempDir()
	tfFile := filepath.Join(tmpDir, "main.tf")

	content := `
module "nested" {
  source = "git::https://github.com/user/repo.git//modules/nested?ref=v1.0.0"
}
`
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0o644))

	err := RewriteModuleSource(tfFile, "nested", "./modules/def456/modules/nested")
	require.NoError(t, err)

	modules, err := ExtractModuleBlocks(tfFile)
	require.NoError(t, err)
	require.Len(t, modules, 1)

	assert.Equal(t, "./modules/def456/modules/nested", modules[0].Source)
}

func TestRewriteModuleSource_InvalidHCL(t *testing.T) {
	tmpDir := t.TempDir()
	tfFile := filepath.Join(tmpDir, "invalid.tf")

	content := `
module "broken" {
  source = "incomplete
}
`
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0o644))

	err := RewriteModuleSource(tfFile, "broken", "./vendor/broken")
	assert.Error(t, err)
}

func TestRewriteModuleSource_NonExistentFile(t *testing.T) {
	err := RewriteModuleSource("/nonexistent/file.tf", "vpc", "./vendor/vpc")
	assert.Error(t, err)
}

func TestRewriteModuleSource_PreservesOtherBlocks(t *testing.T) {
	tmpDir := t.TempDir()
	tfFile := filepath.Join(tmpDir, "main.tf")

	content := `
variable "region" {
  type    = string
  default = "us-west-2"
}

module "vpc" {
  source = "terraform-aws-modules/vpc/aws"
}

resource "aws_instance" "example" {
  ami           = "ami-12345678"
  instance_type = "t2.micro"
}

output "vpc_id" {
  value = module.vpc.vpc_id
}
`
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0o644))

	err := RewriteModuleSource(tfFile, "vpc", "./vendor/vpc")
	require.NoError(t, err)

	// Read back the file
	data, err := os.ReadFile(tfFile)
	require.NoError(t, err)

	result := string(data)

	// Verify all other blocks are preserved
	assert.Contains(t, result, `variable "region"`)
	assert.Contains(t, result, `resource "aws_instance" "example"`)
	assert.Contains(t, result, `output "vpc_id"`)
	assert.Contains(t, result, `instance_type = "t2.micro"`)
}

func TestRewriteModuleSourceAndDropVersion(t *testing.T) {
	tmpDir := t.TempDir()
	tfFile := filepath.Join(tmpDir, "main.tf")

	content := `
module "vpc" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "5.21.0"
}

module "eks" {
  source  = "terraform-aws-modules/eks/aws"
  version = "20.0.0"
}
`
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0o644))

	// Rewrite vpc from registry source to a local vendored path, dropping version.
	err := RewriteModuleSourceAndDropVersion(tfFile, "vpc", "./_vendor/pkg-abc")
	require.NoError(t, err)

	blocks, err := ExtractModuleBlocks(tfFile)
	require.NoError(t, err)
	require.Len(t, blocks, 2)

	// vpc: source rewritten to the local vendor path and version stripped (item 1).
	vpc := blocks[0]
	assert.Equal(t, "vpc", vpc.Name)
	assert.Equal(t, "./_vendor/pkg-abc", vpc.Source)
	assert.Empty(t, vpc.Version, "version must be dropped when rewriting a registry source to local")

	// eks: untouched (rewrite targeted only `vpc`).
	eks := blocks[1]
	assert.Equal(t, "eks", eks.Name)
	assert.Equal(t, "terraform-aws-modules/eks/aws", eks.Source)
	assert.Equal(t, "20.0.0", eks.Version)

	// Provenance: the original pinned version must survive as a comment in the file.
	data, err := os.ReadFile(tfFile)
	require.NoError(t, err)
	assert.Contains(t, string(data), `# version = "5.21.0" (pinned by tofupress)`)
}

// TestRewriteModuleSourceAndDropVersion_NoVersionAttr is the no-op contract:
// when a registry block was already rewritten (no `version`), the call must
// still succeed and rewrite `source` without dropping provenance it never had.
func TestRewriteModuleSourceAndDropVersion_NoVersionAttr(t *testing.T) {
	tmpDir := t.TempDir()
	tfFile := filepath.Join(tmpDir, "main.tf")

	content := `
module "vpc" {
  source = "terraform-aws-modules/vpc/aws"
}
`
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0o644))

	err := RewriteModuleSourceAndDropVersion(tfFile, "vpc", "./_vendor/pkg-xyz")
	require.NoError(t, err)

	blocks, err := ExtractModuleBlocks(tfFile)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	assert.Equal(t, "./_vendor/pkg-xyz", blocks[0].Source)
	assert.Empty(t, blocks[0].Version)

	data, err := os.ReadFile(tfFile)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "pinned by tofupress",
		"no provenance comment should be emitted when the version attribute was absent")
}
