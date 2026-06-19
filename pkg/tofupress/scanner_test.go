package tofupress

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFindTerraformFiles(t *testing.T) {
	// Create a temp directory with various files
	tmpDir := t.TempDir()

	// Create .tf files
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "main.tf"), []byte("# main"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "variables.tf"), []byte("# vars"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "outputs.tf"), []byte("# outputs"), 0644))

	// Create non-.tf files
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("# readme"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "data.json"), []byte("{}"), 0644))

	// Create hidden directory (should be skipped)
	hiddenDir := filepath.Join(tmpDir, ".terraform")
	require.NoError(t, os.MkdirAll(hiddenDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(hiddenDir, "hidden.tf"), []byte("# hidden"), 0644))

	// Create subdirectory (should be skipped by FindTerraformFiles)
	subDir := filepath.Join(tmpDir, "modules")
	require.NoError(t, os.MkdirAll(subDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(subDir, "sub.tf"), []byte("# sub"), 0644))

	files, err := FindTerraformFiles(tmpDir)
	require.NoError(t, err)

	// Should find exactly 3 .tf files in the root
	assert.Len(t, files, 3)

	// Check that we got the right files
	fileNames := make([]string, len(files))
	for i, f := range files {
		fileNames[i] = filepath.Base(f)
	}
	assert.Contains(t, fileNames, "main.tf")
	assert.Contains(t, fileNames, "variables.tf")
	assert.Contains(t, fileNames, "outputs.tf")
	assert.NotContains(t, fileNames, "hidden.tf")
	assert.NotContains(t, fileNames, "sub.tf")
}

func TestFindTerraformFiles_EmptyDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	files, err := FindTerraformFiles(tmpDir)
	require.NoError(t, err)
	assert.Empty(t, files)
}

func TestFindTerraformFiles_NonExistentDirectory(t *testing.T) {
	files, err := FindTerraformFiles("/nonexistent/path")
	assert.Error(t, err)
	assert.Nil(t, files)
}

func TestExtractModuleBlocks(t *testing.T) {
	tmpDir := t.TempDir()
	tfFile := filepath.Join(tmpDir, "main.tf")

	content := `
module "vpc" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "3.0.0"

  name = "my-vpc"
  cidr = "10.0.0.0/16"
}

module "eks" {
  source = "terraform-aws-modules/eks/aws"

  cluster_name = "my-cluster"
}

resource "aws_instance" "example" {
  ami           = "ami-12345678"
  instance_type = "t2.micro"
}
`
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0644))

	modules, err := ExtractModuleBlocks(tfFile)
	require.NoError(t, err)

	assert.Len(t, modules, 2)

	// First module
	assert.Equal(t, "vpc", modules[0].Name)
	assert.Equal(t, "terraform-aws-modules/vpc/aws", modules[0].Source)

	// Second module
	assert.Equal(t, "eks", modules[1].Name)
	assert.Equal(t, "terraform-aws-modules/eks/aws", modules[1].Source)
}

func TestExtractModuleBlocks_NoModules(t *testing.T) {
	tmpDir := t.TempDir()
	tfFile := filepath.Join(tmpDir, "main.tf")

	content := `
resource "aws_instance" "example" {
  ami           = "ami-12345678"
  instance_type = "t2.micro"
}

variable "name" {
  type = string
}
`
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0644))

	modules, err := ExtractModuleBlocks(tfFile)
	require.NoError(t, err)
	assert.Empty(t, modules)
}

func TestExtractModuleBlocks_LocalSources(t *testing.T) {
	tmpDir := t.TempDir()
	tfFile := filepath.Join(tmpDir, "main.tf")

	content := `
module "local1" {
  source = "./modules/vpc"
}

module "local2" {
  source = "../shared/modules/eks"
}

module "remote" {
  source = "git::https://github.com/example/repo.git"
}
`
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0644))

	modules, err := ExtractModuleBlocks(tfFile)
	require.NoError(t, err)

	assert.Len(t, modules, 3)
	assert.Equal(t, "./modules/vpc", modules[0].Source)
	assert.Equal(t, "../shared/modules/eks", modules[1].Source)
	assert.Equal(t, "git::https://github.com/example/repo.git", modules[2].Source)
}

func TestExtractModuleBlocks_ComplexSources(t *testing.T) {
	tmpDir := t.TempDir()
	tfFile := filepath.Join(tmpDir, "main.tf")

	content := `
module "git_with_ref" {
  source = "git::https://github.com/user/repo.git?ref=v1.0.0"
}

module "git_with_subdir" {
  source = "git::https://github.com/user/repo.git//modules/vpc?ref=main"
}

module "registry" {
  source  = "hashicorp/consul/aws"
  version = "0.1.0"
}

module "s3" {
  source = "s3::https://s3-eu-west-1.amazonaws.com/bucket/terraform-modules/module.zip"
}
`
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0644))

	modules, err := ExtractModuleBlocks(tfFile)
	require.NoError(t, err)

	assert.Len(t, modules, 4)
	assert.Equal(t, "git::https://github.com/user/repo.git?ref=v1.0.0", modules[0].Source)
	assert.Equal(t, "git::https://github.com/user/repo.git//modules/vpc?ref=main", modules[1].Source)
	assert.Equal(t, "hashicorp/consul/aws", modules[2].Source)
	assert.Equal(t, "s3::https://s3-eu-west-1.amazonaws.com/bucket/terraform-modules/module.zip", modules[3].Source)
}

func TestExtractModuleBlocks_EmptyFile(t *testing.T) {
	tmpDir := t.TempDir()
	tfFile := filepath.Join(tmpDir, "empty.tf")

	require.NoError(t, os.WriteFile(tfFile, []byte(""), 0644))

	modules, err := ExtractModuleBlocks(tfFile)
	require.NoError(t, err)
	assert.Empty(t, modules)
}

func TestExtractModuleBlocks_InvalidHCL(t *testing.T) {
	tmpDir := t.TempDir()
	tfFile := filepath.Join(tmpDir, "invalid.tf")

	content := `
module "broken" {
  source = "incomplete
}
`
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0644))

	modules, err := ExtractModuleBlocks(tfFile)
	assert.Error(t, err)
	assert.Nil(t, modules)
}

func TestExtractModuleBlocks_MissingSource(t *testing.T) {
	tmpDir := t.TempDir()
	tfFile := filepath.Join(tmpDir, "main.tf")

	content := `
module "no_source" {
  version = "1.0.0"
}
`
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0644))

	modules, err := ExtractModuleBlocks(tfFile)
	require.NoError(t, err)

	// Should return the module but with empty source
	assert.Len(t, modules, 1)
	assert.Equal(t, "no_source", modules[0].Name)
	assert.Empty(t, modules[0].Source)
}

func TestExtractModuleBlocks_VariableSource(t *testing.T) {
	tmpDir := t.TempDir()
	tfFile := filepath.Join(tmpDir, "main.tf")

	content := `
module "dynamic" {
  source = var.module_source
}
`
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0644))

	modules, err := ExtractModuleBlocks(tfFile)
	require.NoError(t, err)

	// Should skip modules with variable sources
	assert.Empty(t, modules)
}

func TestModuleBlock_String(t *testing.T) {
	mb := ModuleBlock{
		Name:   "test",
		Source: "./modules/test",
	}

	str := mb.String()
	assert.Contains(t, str, "test")
	assert.Contains(t, str, "./modules/test")
}
