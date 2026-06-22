//nolint:gosec // test files use standard permissions and safe paths
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
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "main.tf"), []byte("# main"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "variables.tf"), []byte("# vars"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "outputs.tf"), []byte("# outputs"), 0o644))

	// Create non-.tf files
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("# readme"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "data.json"), []byte("{}"), 0o644))

	// Create hidden directory (should be skipped)
	hiddenDir := filepath.Join(tmpDir, ".terraform")
	require.NoError(t, os.MkdirAll(hiddenDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(hiddenDir, "hidden.tf"), []byte("# hidden"), 0o644))

	// Create subdirectory (should be skipped by FindTerraformFiles)
	subDir := filepath.Join(tmpDir, "modules")
	require.NoError(t, os.MkdirAll(subDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(subDir, "sub.tf"), []byte("# sub"), 0o644))

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

func TestFindTerraformFiles_IncludesTofuAndPrioritizesSameBasename(t *testing.T) {
	tmpDir := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "main.tf"), []byte("# tf main"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "main.tofu"), []byte("# tofu main"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "variables.tf"), []byte("# vars"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "outputs.tofu"), []byte("# outputs"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("# readme"), 0o644))

	subDir := filepath.Join(tmpDir, "modules")
	require.NoError(t, os.MkdirAll(subDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(subDir, "child.tofu"), []byte("# child"), 0o644))

	files, err := FindTerraformFiles(tmpDir)
	require.NoError(t, err)

	fileNames := make([]string, 0, len(files))
	for _, file := range files {
		fileNames = append(fileNames, filepath.Base(file))
	}

	assert.ElementsMatch(t, []string{"main.tofu", "variables.tf", "outputs.tofu"}, fileNames)
	assert.NotContains(t, fileNames, "main.tf")
	assert.NotContains(t, fileNames, "child.tofu")
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
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0o644))

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
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0o644))

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
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0o644))

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
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0o644))

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

	require.NoError(t, os.WriteFile(tfFile, []byte(""), 0o644))

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
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0o644))

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
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0o644))

	modules, err := ExtractModuleBlocks(tfFile)
	require.NoError(t, err)

	// Should return the module but with empty source
	assert.Len(t, modules, 1)
	assert.Equal(t, "no_source", modules[0].Name)
	assert.Empty(t, modules[0].Source)
	assert.True(t, modules[0].MissingSource, "module without source should have MissingSource=true")
}

func TestExtractModuleBlocks_VariableSource(t *testing.T) {
	tmpDir := t.TempDir()
	tfFile := filepath.Join(tmpDir, "main.tf")

	content := `
module "dynamic" {
  source = var.module_source
}
`
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0o644))

	modules, err := ExtractModuleBlocks(tfFile)
	require.NoError(t, err)

	// Should return modules with DynamicSource flag set
	require.Len(t, modules, 1)
	assert.True(t, modules[0].DynamicSource, "module with variable source should have DynamicSource=true")
	assert.Equal(t, "dynamic", modules[0].Name)
	assert.Empty(t, modules[0].Source)
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

func TestExtractModuleBlocks_HeredocSource(t *testing.T) {
	// Heredoc syntax is valid Terraform for module sources.
	// Regression: the HCL parser evaluates the heredoc body literally,
	// including leading whitespace from indented heredocs (<<-).
	// This means the source string is "\n    ./child\n  " instead of "./child",
	// which causes the resolver to look for a non-existent directory.
	tmpDir := t.TempDir()
	tfFile := filepath.Join(tmpDir, "main.tf")

	content := `
module "child" {
  source = <<-EOT
    ./child
  EOT
}
`
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0o644))

	modules, err := ExtractModuleBlocks(tfFile)
	require.NoError(t, err)

	require.Len(t, modules, 1)
	assert.Equal(t, "child", modules[0].Name)
	// Bug: heredoc sources include embedded whitespace.
	// The source will NOT be clean "./child" — it will contain newlines and
	// indentation from the heredoc body.
	assert.NotEqual(t, "./child", modules[0].Source,
		"heredoc source should be trimmed to just the path")
}

func TestExtractModuleBlocks_DuplicateModuleNames(t *testing.T) {
	// Duplicate module names in the same file are rejected by OpenTofu/Terraform.
	// The scanner should extract both blocks so the resolver can detect the
	// conflict and produce a clear error.
	tmpDir := t.TempDir()
	tfFile := filepath.Join(tmpDir, "main.tf")

	content := `
module "vpc" {
  source = "./child"
}
module "vpc" {
  source = "./child"
}
`
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0o644))

	modules, err := ExtractModuleBlocks(tfFile)
	require.NoError(t, err)

	// Both blocks should be extracted so the resolver can detect the conflict.
	assert.Len(t, modules, 2, "both duplicate module blocks should be extracted")
	assert.Equal(t, "vpc", modules[0].Name)
	assert.Equal(t, "vpc", modules[1].Name)
}

func TestExtractModuleBlocks_UTF8ModuleNames(t *testing.T) {
	// Module names can contain Unicode characters.
	tmpDir := t.TempDir()
	tfFile := filepath.Join(tmpDir, "main.tf")

	content := `
module "mödulé_αβ" {
  source = "./child"
}
`
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0o644))

	modules, err := ExtractModuleBlocks(tfFile)
	require.NoError(t, err)

	require.Len(t, modules, 1)
	assert.Equal(t, "mödulé_αβ", modules[0].Name)
	assert.Equal(t, "./child", modules[0].Source)
}

func TestExtractModuleBlocks_LongModuleName(t *testing.T) {
	tmpDir := t.TempDir()
	tfFile := filepath.Join(tmpDir, "main.tf")

	longName := "this_is_a_very_long_module_name_that_exceeds_typical_naming_conventions"
	content := `
module "` + longName + `" {
  source = "./child"
}
`
	require.NoError(t, os.WriteFile(tfFile, []byte(content), 0o644))

	modules, err := ExtractModuleBlocks(tfFile)
	require.NoError(t, err)

	require.Len(t, modules, 1)
	assert.Equal(t, longName, modules[0].Name)
}

func TestFindTerraformFiles_SkipsHCLFiles(t *testing.T) {
	// .hcl files are currently not included when scanning for .tf/.tofu files.
	// This test documents the current behavior.
	tmpDir := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "main.tf"), []byte("# main"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "config.hcl"), []byte("# hcl config"), 0o644))

	files, err := FindTerraformFiles(tmpDir)
	require.NoError(t, err)

	fileNames := make([]string, len(files))
	for i, f := range files {
		fileNames[i] = filepath.Base(f)
	}

	assert.Contains(t, fileNames, "main.tf")
	assert.NotContains(t, fileNames, "config.hcl", ".hcl files are currently excluded from scanning")
}

func TestFindTerraformFiles_OverrideNamedFiles(t *testing.T) {
	// Files named "override.tf" should be discovered alongside "main.tf".
	// (Terraform override files conventionally use _override.tf suffix, but
	// any .tf file in the root module directory is valid and must be scanned.)
	tmpDir := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "main.tf"), []byte("# main"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "override.tf"), []byte("# override"), 0o644))

	files, err := FindTerraformFiles(tmpDir)
	require.NoError(t, err)

	fileNames := make([]string, len(files))
	for i, f := range files {
		fileNames[i] = filepath.Base(f)
	}

	assert.Contains(t, fileNames, "main.tf")
	assert.Contains(t, fileNames, "override.tf", "all .tf files in the root should be discovered")
}
