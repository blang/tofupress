//nolint:gosec // test files use standard permissions
package tofupress

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseStripMode(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		want       StripMode
		aggressive bool
	}{
		{name: "empty defaults to module dir", raw: "", want: StripModeModuleDir},
		{name: "module dir", raw: "module-dir", want: StripModeModuleDir},
		{name: "none", raw: "none", want: StripModeNone},
		{name: "config only", raw: "config-only", want: StripModeConfigOnly, aggressive: true},
		{name: "tf only alias", raw: "tf-only", want: StripModeConfigOnly, aggressive: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseStripMode(tt.raw)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.aggressive, got.IsAggressive())
		})
	}
}

func TestParseStripModeRejectsUnknown(t *testing.T) {
	_, err := ParseStripMode("delete-everything")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported strip mode")
	assert.Contains(t, err.Error(), "none")
	assert.Contains(t, err.Error(), "module-dir")
	assert.Contains(t, err.Error(), "config-only")
}

func TestPlanStrippingNoneIncludesPackageContent(t *testing.T) {
	rootDir := t.TempDir()
	writeFile(t, rootDir, "main.tf", `output "name" { value = "root" }`)
	writeFile(t, rootDir, "README.md", `docs`)
	writeFile(t, rootDir, "examples/example.tf", `output "example" { value = true }`)
	writeFile(t, rootDir, ".terraform/modules/ignored/main.tf", `ignored`)

	tree := testTree(rootDir)
	plan, err := PlanStripping(context.Background(), tree, StripModeNone)
	require.NoError(t, err)

	assert.True(t, plan.IncludePath(filepath.Join(rootDir, "main.tf"), false))
	assert.True(t, plan.IncludePath(filepath.Join(rootDir, "README.md"), false))
	assert.True(t, plan.IncludePath(filepath.Join(rootDir, "examples", "example.tf"), false))
	assert.False(t, plan.IncludePath(filepath.Join(rootDir, ".terraform", "modules", "ignored", "main.tf"), false))
	assert.Equal(t, StripModeNone, plan.Mode)
	assert.Zero(t, plan.StrippedBytes)
}

func TestPlanStrippingModuleDirIncludesOnlyResolvedModuleDirsAndStaticReads(t *testing.T) {
	packageRoot := t.TempDir()
	writeFile(t, packageRoot, "modules/app/main.tf", `locals { rendered = templatefile("../../shared/templates/app.tftpl", {}) }`)
	writeFile(t, packageRoot, "modules/app/variables.tf", `variable "name" { type = string }`)
	writeFile(t, packageRoot, "modules/worker/main.tf", `output "worker" { value = true }`)
	writeFile(t, packageRoot, "shared/templates/app.tftpl", `hello`)
	writeFile(t, packageRoot, "README.md", `strip me`)
	writeFile(t, packageRoot, "examples/example.tf", `strip me`)

	root := &ModuleNode{Key: "", Name: "root", InstallDir: filepath.Join(packageRoot, "modules", "app"), PackageRoot: packageRoot, IsLocal: true}
	worker := &ModuleNode{Parent: root, Key: "worker", Name: "worker", InstallDir: filepath.Join(packageRoot, "modules", "worker"), PackageRoot: packageRoot, IsLocal: true, Source: ModuleSource{Raw: "../worker", Type: SourceLocal, PackageAddr: "../worker"}}
	root.Children = []*ModuleNode{worker}
	tree := &ResolvedTree{Root: root, AllModules: []*ModuleNode{root, worker}, Packages: map[string]*DownloadedPackage{}}

	plan, err := PlanStripping(context.Background(), tree, StripModeModuleDir)
	require.NoError(t, err)

	assert.True(t, plan.IncludePath(filepath.Join(packageRoot, "modules", "app", "main.tf"), false))
	assert.True(t, plan.IncludePath(filepath.Join(packageRoot, "modules", "worker", "main.tf"), false))
	assert.True(t, plan.IncludePath(filepath.Join(packageRoot, "shared", "templates", "app.tftpl"), false))
	assert.False(t, plan.IncludePath(filepath.Join(packageRoot, "README.md"), false))
	assert.False(t, plan.IncludePath(filepath.Join(packageRoot, "examples", "example.tf"), false))
	assert.Greater(t, plan.StrippedFiles, 0)
	assert.Greater(t, plan.StrippedBytes, int64(0))
}

func TestPlanStrippingModuleDirFallsBackToWholePackageForDynamicRead(t *testing.T) {
	packageRoot := t.TempDir()
	writeFile(t, packageRoot, "modules/app/main.tf", `variable "name" { type = string }
locals { rendered = file("../shared/${var.name}.txt") }`)
	writeFile(t, packageRoot, "shared/a.txt", `a`)
	writeFile(t, packageRoot, "README.md", `kept by fallback`)

	root := &ModuleNode{Key: "", Name: "root", InstallDir: filepath.Join(packageRoot, "modules", "app"), PackageRoot: packageRoot, IsLocal: true}
	tree := &ResolvedTree{Root: root, AllModules: []*ModuleNode{root}, Packages: map[string]*DownloadedPackage{}}

	plan, err := PlanStripping(context.Background(), tree, StripModeModuleDir)
	require.NoError(t, err)

	assert.True(t, plan.IncludePath(filepath.Join(packageRoot, "README.md"), false), "dynamic read should keep whole legal package scope")
	assert.Len(t, plan.FilesystemFunctions, 1)
	assert.Equal(t, "dynamic-package-fallback", plan.FilesystemFunctions[0].Handling)
}

func TestPlanStrippingConfigOnlyWarnsAndDoesNotFallbackForDynamicRead(t *testing.T) {
	packageRoot := t.TempDir()
	writeFile(t, packageRoot, "modules/app/main.tf", `variable "name" { type = string }
locals { rendered = file("../shared/${var.name}.txt") }`)
	writeFile(t, packageRoot, "shared/a.txt", `strip in aggressive mode`)

	root := &ModuleNode{Key: "", Name: "root", InstallDir: filepath.Join(packageRoot, "modules", "app"), PackageRoot: packageRoot, IsLocal: true}
	tree := &ResolvedTree{Root: root, AllModules: []*ModuleNode{root}, Packages: map[string]*DownloadedPackage{}}

	plan, err := PlanStripping(context.Background(), tree, StripModeConfigOnly)
	require.NoError(t, err)

	assert.True(t, plan.IncludePath(filepath.Join(packageRoot, "modules", "app", "main.tf"), false))
	assert.False(t, plan.IncludePath(filepath.Join(packageRoot, "shared", "a.txt"), false))
	require.NotEmpty(t, plan.Warnings)
	assert.Contains(t, plan.Warnings[0].Message, "filesystem reads were detected")
}

func TestPlanStrippingModuleDirKeepsFilesetMatches(t *testing.T) {
	rootDir := t.TempDir()
	writeFile(t, rootDir, "main.tf", `locals { policies = fileset("policies", "*.json") }`)
	writeFile(t, rootDir, "policies/keep.json", `{}`)
	writeFile(t, rootDir, "policies/drop.txt", `drop`)

	tree := testTree(rootDir)
	plan, err := PlanStripping(context.Background(), tree, StripModeModuleDir)
	require.NoError(t, err)

	assert.True(t, plan.IncludePath(filepath.Join(rootDir, "policies", "keep.json"), false))
	assert.False(t, plan.IncludePath(filepath.Join(rootDir, "policies", "drop.txt"), false))
}

// TestPlanStrippingModuleDirWarnsOnExcludedRemotePackageSibling (review item 4)
// verifies that a downloaded remote package containing an unreached sibling
// directory (the kind a conditional ../sibling ref would hit at runtime)
// produces a loud warning naming the excluded directory and the package, so
// the silent content-loss failure mode is surfaced to the user.
func TestPlanStrippingModuleDirWarnsOnExcludedRemotePackageSibling(t *testing.T) {
	rootDir := t.TempDir()
	writeFile(t, rootDir, "main.tf", `module "ext" { source = "git::https://example.com/pkg-a.git" }`)

	// Simulate the downloaded remote package: it has a reached dir (the entry)
	// and an unreached sibling dir the resolver never descended into.
	pkgRoot := t.TempDir()
	writeFile(t, pkgRoot, "main.tf", `output "entry" { value = "ok" }`)
	writeFile(t, filepath.Join(pkgRoot, "modules", "helper"), "main.tf", `resource "x" "y" {}`)

	root := &ModuleNode{Key: "", Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}
	ext := &ModuleNode{
		Key:         "ext",
		Name:        "ext",
		Parent:      root,
		InstallDir:  pkgRoot,
		PackageRoot: pkgRoot,
		IsRemote:    true,
		Source:      ModuleSource{Raw: "git::https://example.com/pkg-a.git", PackageAddr: "git::https://example.com/pkg-a.git", Type: SourceGit},
	}
	root.Children = []*ModuleNode{ext}
	tree := &ResolvedTree{
		Root:       root,
		AllModules: []*ModuleNode{root, ext},
		VendorDir:  "_vendor",
		Packages: map[string]*DownloadedPackage{
			"git::https://example.com/pkg-a.git": {
				PackageAddr: "git::https://example.com/pkg-a.git",
				LocalDir:    pkgRoot,
			},
		},
	}

	plan, err := PlanStripping(context.Background(), tree, StripModeModuleDir)
	require.NoError(t, err)

	var found bool
	for _, w := range plan.Warnings {
		if strings.Contains(w.Message, "modules") && strings.Contains(w.Message, "pkg-a.git") &&
			strings.Contains(w.Message, "review item 4") {
			found = true
			break
		}
	}
	assert.True(t, found,
		"item 4: expected a warning about the excluded 'modules' sibling in remote package pkg-a.git, got %+v",
		plan.Warnings)
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func testTree(rootDir string) *ResolvedTree {
	root := &ModuleNode{Key: "", Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}
	return &ResolvedTree{Root: root, AllModules: []*ModuleNode{root}, Packages: map[string]*DownloadedPackage{}}
}
