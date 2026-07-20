//nolint:gosec // test files use standard permissions
package tofupress

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
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

// TestDetectModuleFilesystemFunctions_PathModuleRiskSignals (ADR-0001 signal #2)
// verifies the detector emits a non-static path.module/path.root risk-signal ref
// for the headliner classes a file() detector cannot see: plain attributes,
// provisioner interpreter/commands, and ${path.root} templates. It also pins
// the boundaries: filesystem-function-call args are NOT double-counted as
// signal #2 (their static match is precise), a trailing slash is required
// (bare ${path.module} is not a file ref), and an escaped interpolation is a
// literal string, not a signal.
func TestDetectModuleFilesystemFunctions_PathModuleRiskSignals(t *testing.T) {
	moduleDir := t.TempDir()
	rootDir := filepath.Dir(moduleDir)
	content := `
locals {
  build_dir   = "${path.module}/build"
  root_cfg    = "${path.root}/global.yaml"
  bare_module = "${path.module}"
  escaped     = "$${path.module}/escaped.txt"
  static_file = file("${path.module}/data.txt")
}
resource "null_resource" "archive" {
  provisioner "local-exec" {
    interpreter = [local.python, "${path.module}/package.py", "build"]
    command     = "cd ${path.module}/src && go build"
  }
}
locals { python = "python3" }
`
	require.NoError(t, os.WriteFile(filepath.Join(moduleDir, "main.tf"), []byte(content), 0o644))

	module := &ModuleNode{Key: "child", Name: "child", InstallDir: moduleDir, PackageRoot: rootDir, IsLocal: true}
	refs, err := DetectModuleFilesystemFunctions(rootDir, module)
	require.NoError(t, err)

	byFunction := make(map[string][]FilesystemFunctionRef)
	for _, ref := range refs {
		byFunction[ref.Function] = append(byFunction[ref.Function], ref)
	}

	// Plain attribute referencing a module-relative file/dir.
	pathModuleRefs := byFunction["path.module"]
	require.NotEmpty(t, pathModuleRefs, "expected path.module risk-signal refs")
	var foundBuildDir, foundInterpreter, foundCommand, foundBareAggressive bool
	for _, ref := range pathModuleRefs {
		assert.Equal(t, refKindPathTemplateRisk, ref.Kind, "path.module ref Kind discriminator")
		assert.False(t, ref.Static, "path.module risk signal must be non-static")
		assert.Equal(t, handlingDynamicFallback, ref.Handling)
		switch {
		case strings.Contains(ref.RawPath, "${path.module}/build"):
			foundBuildDir = true
		case strings.Contains(ref.RawPath, "${path.module}/package.py"):
			foundInterpreter = true
		case strings.Contains(ref.RawPath, "${path.module}/src"):
			foundCommand = true
		case ref.RawPath == "${path.module}":
			foundBareAggressive = true
		}
	}
	assert.True(t, foundBuildDir, "plain attribute build_dir = \"${path.module}/build\" must be flagged")
	assert.True(t, foundInterpreter, "provisioner interpreter ${path.module}/package.py must be flagged (Ex 3)")
	assert.True(t, foundCommand, "provisioner command cd ${path.module}/src must be flagged")
	assert.False(t, foundBareAggressive, "bare ${path.module} (no trailing slash) must NOT be flagged — it names the dir, not a file")

	// ${path.root} produces a path.root risk-signal ref.
	pathRootRefs := byFunction["path.root"]
	require.NotEmpty(t, pathRootRefs, "expected a path.root risk-signal ref")
	for _, ref := range pathRootRefs {
		assert.Equal(t, refKindPathTemplateRisk, ref.Kind)
		assert.False(t, ref.Static)
		assert.Equal(t, handlingDynamicFallback, ref.Handling)
	}
	assert.True(t, func() bool {
		for _, ref := range pathRootRefs {
			if strings.Contains(ref.RawPath, "${path.root}/global.yaml") {
				return true
			}
		}
		return false
	}(), "locals root_cfg = \"${path.root}/global.yaml\" must be flagged")

	// The static file("${path.module}/data.txt") is signal #1, NOT signal #2:
	// it appears once as a `file` filesystem-function ref, and is NOT
	// double-counted as a path.module risk-signal ref.
	fileRefs := byFunction["file"]
	require.Len(t, fileRefs, 1, "exactly one file() call, counted once as a filesystem-function ref")
	assert.Equal(t, refKindFilesystemFunction, fileRefs[0].Kind)
	assert.True(t, fileRefs[0].Static, "static file(\"${path.module}/data.txt\") is precise (signal #1)")

	// Escaped interpolation is a literal string, not a signal of either kind.
	for _, ref := range refs {
		assert.NotContains(t, ref.RawPath, "escaped.txt",
			"escaped $${path.module}/escaped.txt is a literal, must not be flagged")
	}
}

// TestRangeEndIndex_Contains pins the O(log N + k) replaceent for the former
// linear rangeContainedInAny (review nit #5): containment must be correct for
// nested filesystem-call arg ranges, non-contained siblings, and the
// edge case where an outer wider range contains a query the narrower inner
// range does not. hcl.Range values are constructed by byte-offset bookkeeping
// so the index's End.Byte sort + binary search is exercisable directly.
func TestRangeEndIndex_Contains(t *testing.T) {
	file := "main.tf"
	mkRange := func(start, end int) hcl.Range {
		return hcl.Range{
			Filename: file,
			Start:    hcl.Pos{Line: 1, Column: 1, Byte: start},
			End:      hcl.Pos{Line: 1, Column: 1, Byte: end},
		}
	}
	// outer [0,100], inner [10,20] — nested, both sorted by End via the index.
	outer := mkRange(0, 100)
	inner := mkRange(10, 20)
	idx := newRangeEndIndex([]hcl.Range{outer, inner})

	// query inside inner → contained by both; first hit wins.
	assert.True(t, idx.contains(mkRange(12, 18)), "inside inner must be contained")
	// query spanning beyond inner end but within outer — only outer contains; this
	// is the edge case a naive "rightmost-start binary search" would miss.
	assert.True(t, idx.contains(mkRange(15, 25)), "query past inner.End but within outer must be contained (nested edge case)")
	// query outside both.
	assert.False(t, idx.contains(mkRange(101, 110)), "past outer.End must not be contained")
	// query in a different file is never contained.
	otherFile := hcl.Range{
		Filename: "other.tf",
		Start:    hcl.Pos{Line: 1, Column: 1, Byte: 5},
		End:      hcl.Pos{Line: 1, Column: 1, Byte: 15},
	}
	assert.False(t, idx.contains(otherFile), "different filename must not be contained")
	// empty index contains nothing.
	assert.False(t, newRangeEndIndex(nil).contains(mkRange(0, 10)), "empty index contains nothing")
}

// BenchmarkDetectModuleFilesystemFunctions measures detector throughput so
// the review nit #4 (fileBytes threaded instead of re-reading disk per
// template) and nit #5 (rangeEndIndex vs linear scan) are observable. Run
// with: go test -bench=BenchmarkDetectModuleFilesystemFunctions -run=^$ ./pkg/tofupress/
func BenchmarkDetectModuleFilesystemFunctions(b *testing.B) {
	rootDir := b.TempDir()
	// A config with many templates and several file() calls to exercise both
	// the raw-text extraction path and the containment index.
	var sb strings.Builder
	sb.WriteString(`locals {` + "\n")
	for i := range 50 {
		fmt.Fprintf(&sb, `  p%d = file("${path.module}/data%d.txt")`+"\n", i, i)
		fmt.Fprintf(&sb, `  r%d = "${path.module}/build%d"`+"\n", i, i)
	}
	sb.WriteString(`}` + "\n")
	require.NoError(b, os.WriteFile(filepath.Join(rootDir, "main.tf"), []byte(sb.String()), 0o644))
	module := &ModuleNode{Key: "root", Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := DetectModuleFilesystemFunctions(rootDir, module); err != nil {
			b.Fatal(err)
		}
	}
}
