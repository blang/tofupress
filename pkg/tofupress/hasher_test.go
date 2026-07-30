//nolint:gosec // test files use standard permissions and safe paths
package tofupress

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHashModule_EmptyDirectory(t *testing.T) {
	tmpDir := t.TempDir()

	hash, err := HashModule(tmpDir)
	require.NoError(t, err)
	// Empty directory should produce a consistent hash
	assert.Len(t, hash, 64) // SHA-256 hex string is 64 chars
}

func TestHashModule_NonExistentDirectory(t *testing.T) {
	_, err := HashModule("/nonexistent/path")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to read directory")
}

func TestHashModule_SingleTerraformFile(t *testing.T) {
	tmpDir := t.TempDir()
	tfFile := filepath.Join(tmpDir, "main.tf")
	err := os.WriteFile(tfFile, []byte(`
resource "null_resource" "test" {
  triggers = {
    value = "test"
  }
}
`), 0o644)
	require.NoError(t, err)

	hash, err := HashModule(tmpDir)
	require.NoError(t, err)
	assert.Len(t, hash, 64)
}

func TestHashModule_MultipleTerraformFiles(t *testing.T) {
	tmpDir := t.TempDir()

	// Create multiple .tf files
	mainTf := filepath.Join(tmpDir, "main.tf")
	err := os.WriteFile(mainTf, []byte(`
resource "null_resource" "main" {}
`), 0o644)
	require.NoError(t, err)

	varsTf := filepath.Join(tmpDir, "variables.tf")
	err = os.WriteFile(varsTf, []byte(`
variable "name" {
  type = string
}
`), 0o644)
	require.NoError(t, err)

	outputsTf := filepath.Join(tmpDir, "outputs.tf")
	err = os.WriteFile(outputsTf, []byte(`
output "id" {
  value = null_resource.main.id
}
`), 0o644)
	require.NoError(t, err)

	hash, err := HashModule(tmpDir)
	require.NoError(t, err)
	assert.Len(t, hash, 64)
}

func TestHashModule_ConsistentHash(t *testing.T) {
	tmpDir := t.TempDir()
	tfFile := filepath.Join(tmpDir, "main.tf")
	content := []byte(`
resource "aws_instance" "web" {
  ami           = "ami-12345"
  instance_type = "t2.micro"
}
`)
	err := os.WriteFile(tfFile, content, 0o644)
	require.NoError(t, err)

	// Hash twice, should be identical
	hash1, err := HashModule(tmpDir)
	require.NoError(t, err)

	hash2, err := HashModule(tmpDir)
	require.NoError(t, err)

	assert.Equal(t, hash1, hash2, "HashModule should produce consistent hashes")
}

func TestHashModule_DifferentContentDifferentHash(t *testing.T) {
	tmpDir1 := t.TempDir()
	tfFile1 := filepath.Join(tmpDir1, "main.tf")
	err := os.WriteFile(tfFile1, []byte(`
resource "null_resource" "test1" {}
`), 0o644)
	require.NoError(t, err)

	tmpDir2 := t.TempDir()
	tfFile2 := filepath.Join(tmpDir2, "main.tf")
	err = os.WriteFile(tfFile2, []byte(`
resource "null_resource" "test2" {}
`), 0o644)
	require.NoError(t, err)

	hash1, err := HashModule(tmpDir1)
	require.NoError(t, err)

	hash2, err := HashModule(tmpDir2)
	require.NoError(t, err)

	assert.NotEqual(t, hash1, hash2, "Different content should produce different hashes")
}

func TestHashModule_DifferentSymlinkTargetsDifferentHash(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	for _, dir := range []string{first, second} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("A"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("B"), 0o644))
	}
	if err := os.Symlink("a.txt", filepath.Join(first, "selected.txt")); err != nil {
		t.Skip("symlinks not supported on this system")
	}
	require.NoError(t, os.Symlink("b.txt", filepath.Join(second, "selected.txt")))

	firstHash, err := HashModule(first)
	require.NoError(t, err)
	secondHash, err := HashModule(second)
	require.NoError(t, err)

	assert.NotEqual(t, firstHash, secondHash, "dereferenced archive content must participate in package identity")
}

func TestSnapshotDirectoryRejectsSymlinkOutsidePackage(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	require.NoError(t, os.WriteFile(outside, []byte("secret"), 0o600))
	if err := os.Symlink(outside, filepath.Join(root, "leak.txt")); err != nil {
		t.Skip("symlinks not supported on this system")
	}

	_, err := SnapshotDirectory(root)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "outside package root")
}

func TestHashModule_IncludesNonTerraformFiles(t *testing.T) {
	tmpDir1 := t.TempDir()
	tfFile1 := filepath.Join(tmpDir1, "main.tf")
	err := os.WriteFile(tfFile1, []byte(`
resource "null_resource" "test" {}
`), 0o644)
	require.NoError(t, err)

	tmpDir2 := t.TempDir()
	tfFile2 := filepath.Join(tmpDir2, "main.tf")
	err = os.WriteFile(tfFile2, []byte(`
resource "null_resource" "test" {}
`), 0o644)
	require.NoError(t, err)

	// Add non-.tf files to tmpDir2
	readmeFile := filepath.Join(tmpDir2, "README.md")
	err = os.WriteFile(readmeFile, []byte("# Test Module"), 0o644)
	require.NoError(t, err)

	jsonFile := filepath.Join(tmpDir2, "metadata.json")
	err = os.WriteFile(jsonFile, []byte(`{"name": "test"}`), 0o644)
	require.NoError(t, err)

	hash1, err := HashModule(tmpDir1)
	require.NoError(t, err)

	hash2, err := HashModule(tmpDir2)
	require.NoError(t, err)

	assert.NotEqual(t, hash1, hash2, "Non-.tf files should affect the hash (whole-directory hashing)")
}

func TestSnapshotDirectoryIncludesNonTerraformFilesAndSubdirectories(t *testing.T) {
	baseDir := t.TempDir()
	withExtraDir := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(baseDir, "main.tf"), []byte("resource \"null_resource\" \"test\" {}"), 0o644))

	require.NoError(t, os.WriteFile(filepath.Join(withExtraDir, "main.tf"), []byte("resource \"null_resource\" \"test\" {}"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(withExtraDir, "README.md"), []byte("# module docs"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(withExtraDir, "templates"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(withExtraDir, "templates", "userdata.tftpl"), []byte("hello ${name}"), 0o644))

	baseSnapshot, err := SnapshotDirectory(baseDir)
	require.NoError(t, err)
	extraSnapshot, err := SnapshotDirectory(withExtraDir)
	require.NoError(t, err)

	assert.NotEqual(t, baseSnapshot.Hash, extraSnapshot.Hash)
	assert.Equal(t, 1, baseSnapshot.FileCount)
	assert.Equal(t, 3, extraSnapshot.FileCount)
	assert.Greater(t, extraSnapshot.TotalBytes, baseSnapshot.TotalBytes)
}

func TestSnapshotDirectoryIgnoresGeneratedAndVCSDirectories(t *testing.T) {
	baseDir := t.TempDir()
	withGeneratedDir := t.TempDir()

	for _, dir := range []string{baseDir, withGeneratedDir} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte("output \"name\" { value = \"same\" }"), 0o644))
	}

	require.NoError(t, os.MkdirAll(filepath.Join(withGeneratedDir, ".terraform", "modules"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(withGeneratedDir, ".terraform", "modules", "ignored.tf"), []byte("ignored"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(withGeneratedDir, ".git", "objects"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(withGeneratedDir, ".git", "objects", "ignored"), []byte("ignored"), 0o644))

	baseSnapshot, err := SnapshotDirectory(baseDir)
	require.NoError(t, err)
	generatedSnapshot, err := SnapshotDirectory(withGeneratedDir)
	require.NoError(t, err)

	assert.Equal(t, baseSnapshot.Hash, generatedSnapshot.Hash)
	assert.Equal(t, baseSnapshot.FileCount, generatedSnapshot.FileCount)
	assert.Equal(t, baseSnapshot.TotalBytes, generatedSnapshot.TotalBytes)
}

func TestSnapshotDirectoryIncludesExecutableBit(t *testing.T) {
	plainDir := t.TempDir()
	execDir := t.TempDir()

	content := []byte("#!/usr/bin/env sh\necho hello\n")
	require.NoError(t, os.WriteFile(filepath.Join(plainDir, "script.sh"), content, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(execDir, "script.sh"), content, 0o755))

	plainSnapshot, err := SnapshotDirectory(plainDir)
	require.NoError(t, err)
	execSnapshot, err := SnapshotDirectory(execDir)
	require.NoError(t, err)

	assert.NotEqual(t, plainSnapshot.Hash, execSnapshot.Hash)
}

func TestHashModule_OrderIndependent(t *testing.T) {
	// Create two directories with same files in different order
	tmpDir1 := t.TempDir()
	err := os.WriteFile(filepath.Join(tmpDir1, "main.tf"), []byte("main content"), 0o644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(tmpDir1, "variables.tf"), []byte("variables content"), 0o644)
	require.NoError(t, err)

	tmpDir2 := t.TempDir()
	err = os.WriteFile(filepath.Join(tmpDir2, "variables.tf"), []byte("variables content"), 0o644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(tmpDir2, "main.tf"), []byte("main content"), 0o644)
	require.NoError(t, err)

	hash1, err := HashModule(tmpDir1)
	require.NoError(t, err)

	hash2, err := HashModule(tmpDir2)
	require.NoError(t, err)

	assert.Equal(t, hash1, hash2, "Hash should be independent of file creation order")
}

func TestHashModule_IncludesSubdirectories(t *testing.T) {
	tmpDir1 := t.TempDir()
	tfFile1 := filepath.Join(tmpDir1, "main.tf")
	err := os.WriteFile(tfFile1, []byte(`
resource "null_resource" "test" {}
`), 0o644)
	require.NoError(t, err)

	tmpDir2 := t.TempDir()
	tfFile2 := filepath.Join(tmpDir2, "main.tf")
	err = os.WriteFile(tfFile2, []byte(`
resource "null_resource" "test" {}
`), 0o644)
	require.NoError(t, err)

	// Add subdirectory with .tf files to tmpDir2
	subDir := filepath.Join(tmpDir2, "modules")
	err = os.Mkdir(subDir, 0o755)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(subDir, "child.tf"), []byte("child content"), 0o644)
	require.NoError(t, err)

	hash1, err := HashModule(tmpDir1)
	require.NoError(t, err)

	hash2, err := HashModule(tmpDir2)
	require.NoError(t, err)

	assert.NotEqual(t, hash1, hash2, "Subdirectories should affect the hash (whole-directory hashing)")
}
