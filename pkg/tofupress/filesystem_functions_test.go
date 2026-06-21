//nolint:gosec // test files use standard permissions
package tofupress

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDetectModuleFilesystemFunctionsFindsAllVerifiedFunctions(t *testing.T) {
	moduleDir := t.TempDir()
	content := `
locals {
  a = file("a.txt")
  b = filebase64("b.bin")
  c = fileexists("c.txt")
  d = fileset("policies", "*.json")
  e = templatefile("templates/userdata.tftpl", { name = "test" })
  f = filemd5("checksums/file.md5")
  g = filesha1("checksums/file.sha1")
  h = filesha256("checksums/file.sha256")
  i = filesha512("checksums/file.sha512")
  j = filebase64sha256("checksums/file.b64sha256")
  k = filebase64sha512("checksums/file.b64sha512")
  ignored = templatestring("hello ${name}", { name = "world" })
}
`
	require.NoError(t, os.WriteFile(filepath.Join(moduleDir, "main.tf"), []byte(content), 0o644))

	module := &ModuleNode{Key: "root", Name: "root", InstallDir: moduleDir, PackageRoot: moduleDir, IsLocal: true}
	refs, err := DetectModuleFilesystemFunctions(moduleDir, module)
	require.NoError(t, err)

	got := make([]string, 0, len(refs))
	for _, ref := range refs {
		got = append(got, ref.Function)
	}
	sort.Strings(got)

	want := []string{
		"file",
		"filebase64",
		"filebase64sha256",
		"filebase64sha512",
		"fileexists",
		"filemd5",
		"fileset",
		"filesha1",
		"filesha256",
		"filesha512",
		"templatefile",
	}
	sort.Strings(want)
	assert.Equal(t, want, got)
}

func TestDetectModuleFilesystemFunctionsClassifiesStaticAndDynamicPaths(t *testing.T) {
	moduleDir := t.TempDir()
	rootDir := filepath.Dir(moduleDir)
	content := `
variable "name" { type = string }
locals {
  literal_file = file("templates/static.tftpl")
  module_file  = templatefile("${path.module}/templates/module.tftpl", {})
  root_file    = file("${path.root}/root.txt")
  dynamic_file = file("templates/${var.name}.tftpl")
}
`
	require.NoError(t, os.WriteFile(filepath.Join(moduleDir, "main.tf"), []byte(content), 0o644))

	module := &ModuleNode{Key: "child", Name: "child", InstallDir: moduleDir, PackageRoot: rootDir, IsLocal: true}
	refs, err := DetectModuleFilesystemFunctions(rootDir, module)
	require.NoError(t, err)
	require.Len(t, refs, 4)

	byRaw := map[string]FilesystemFunctionRef{}
	for _, ref := range refs {
		byRaw[ref.RawPath] = ref
	}

	assert.True(t, byRaw["templates/static.tftpl"].Static)
	assert.Equal(t, filepath.Join(moduleDir, "templates", "static.tftpl"), byRaw["templates/static.tftpl"].ResolvedBase)
	assert.True(t, byRaw["${path.module}/templates/module.tftpl"].Static)
	assert.Equal(t, filepath.Join(moduleDir, "templates", "module.tftpl"), byRaw["${path.module}/templates/module.tftpl"].ResolvedBase)
	assert.True(t, byRaw["${path.root}/root.txt"].Static)
	assert.Equal(t, filepath.Join(rootDir, "root.txt"), byRaw["${path.root}/root.txt"].ResolvedBase)
	assert.False(t, byRaw[""].Static, "dynamic var path should not be statically resolved")
}

func TestDetectModuleFilesystemFunctionsUsesTofuPriority(t *testing.T) {
	moduleDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(moduleDir, "main.tf"), []byte(`locals { ignored = file("ignored.txt") }`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(moduleDir, "main.tofu"), []byte(`locals { kept = file("kept.txt") }`), 0o644))

	module := &ModuleNode{Key: "root", Name: "root", InstallDir: moduleDir, PackageRoot: moduleDir, IsLocal: true}
	refs, err := DetectModuleFilesystemFunctions(moduleDir, module)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, "kept.txt", refs[0].RawPath)
}

func TestDetectModuleFilesystemFunctionsExpandsStaticFileset(t *testing.T) {
	moduleDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(moduleDir, "policies", "nested"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(moduleDir, "policies", "a.json"), []byte("{}"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(moduleDir, "policies", "b.txt"), []byte("ignore"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(moduleDir, "policies", "nested", "c.json"), []byte("{}"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(moduleDir, "main.tf"), []byte(`locals { policies = fileset("policies", "**/*.json") }`), 0o644))

	module := &ModuleNode{Key: "root", Name: "root", InstallDir: moduleDir, PackageRoot: moduleDir, IsLocal: true}
	refs, err := DetectModuleFilesystemFunctions(moduleDir, module)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.True(t, refs[0].Static)
	assert.Equal(t, []string{
		filepath.Join(moduleDir, "policies", "a.json"),
		filepath.Join(moduleDir, "policies", "nested", "c.json"),
	}, refs[0].IncludedPaths)
}
