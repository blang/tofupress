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
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0644))

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
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0644))

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
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0644))

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
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0644))

	err := RewriteModuleSource(tfFile, "vpc", "./vendor/vpc")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "module \"vpc\" not found")
}

func TestRewriteModuleSource_EmptyFile(t *testing.T) {
	tmpDir := t.TempDir()
	tfFile := filepath.Join(tmpDir, "empty.tf")

	require.NoError(t, os.WriteFile(tfFile, []byte(""), 0644))

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
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0644))

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
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0644))

	// Rewrite local to remote path
	err := RewriteModuleSource(tfFile, "local", "./sourcetree/abc123/modules/local")
	require.NoError(t, err)

	modules, err := ExtractModuleBlocks(tfFile)
	require.NoError(t, err)
	require.Len(t, modules, 1)

	assert.Equal(t, "./sourcetree/abc123/modules/local", modules[0].Source)
}

func TestRewriteModuleSource_WithComplexPath(t *testing.T) {
	tmpDir := t.TempDir()
	tfFile := filepath.Join(tmpDir, "main.tf")

	content := `
module "nested" {
  source = "git::https://github.com/user/repo.git//modules/nested?ref=v1.0.0"
}
`
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0644))

	err := RewriteModuleSource(tfFile, "nested", "./sourcetree/def456/modules/nested")
	require.NoError(t, err)

	modules, err := ExtractModuleBlocks(tfFile)
	require.NoError(t, err)
	require.Len(t, modules, 1)

	assert.Equal(t, "./sourcetree/def456/modules/nested", modules[0].Source)
}

func TestRewriteModuleSource_InvalidHCL(t *testing.T) {
	tmpDir := t.TempDir()
	tfFile := filepath.Join(tmpDir, "invalid.tf")

	content := `
module "broken" {
  source = "incomplete
}
`
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0644))

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
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0644))

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
