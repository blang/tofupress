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
		{name: "empty defaults to optimistic", raw: "", want: StripModeOptimistic},
		{name: "optimistic", raw: "optimistic", want: StripModeOptimistic},
		{name: "module-dir alias", raw: "module-dir", want: StripModeOptimistic},
		{name: "full", raw: "full", want: StripModeFull},
		{name: "none alias", raw: "none", want: StripModeFull},
		{name: "aggressive", raw: "aggressive", want: StripModeAggressive, aggressive: true},
		{name: "config-only alias", raw: "config-only", want: StripModeAggressive, aggressive: true},
		{name: "tf only alias", raw: "tf-only", want: StripModeAggressive, aggressive: true},
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
	assert.Contains(t, err.Error(), "unsupported strip level")
	assert.Contains(t, err.Error(), "full")
	assert.Contains(t, err.Error(), "optimistic")
	assert.Contains(t, err.Error(), "aggressive")
	// Legacy aliases still surfaced for alpha migration.
	assert.Contains(t, err.Error(), "none")
	assert.Contains(t, err.Error(), "module-dir")
	assert.Contains(t, err.Error(), "config-only")
}

func TestStripPlanStatsCountOverlappingPackageFilesOnce(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "root.txt", "root")
	writeFile(t, root, "nested/keep.tf", "keep")
	writeFile(t, root, "nested/drop.bin", "drop")

	outer := newPackageStripPlan(root)
	outer.IncludeAll = true
	innerRoot := filepath.Join(root, "nested")
	inner := newPackageStripPlan(innerRoot)
	inner.includeFile(filepath.Join(innerRoot, "keep.tf"))
	plan := &StripPlan{Packages: map[string]*PackageStripPlan{root: outer, innerRoot: inner}}

	require.NoError(t, plan.computeStats())

	assert.Equal(t, int64(len("root")+len("keep")+len("drop")), plan.OriginalBytes)
	assert.Equal(t, int64(len("root")+len("keep")), plan.FinalBytes)
	assert.Equal(t, int64(len("drop")), plan.StrippedBytes)
	assert.Equal(t, 1, plan.StrippedFiles)
	assert.Equal(t, int64(len("root")), outer.OriginalBytes)
	assert.Equal(t, int64(len("keep")+len("drop")), inner.OriginalBytes)
}

func TestPlanStrippingNoneIncludesPackageContent(t *testing.T) {
	rootDir := t.TempDir()
	writeFile(t, rootDir, "main.tf", `output "name" { value = "root" }`)
	writeFile(t, rootDir, "README.md", `docs`)
	writeFile(t, rootDir, "examples/example.tf", `output "example" { value = true }`)
	writeFile(t, rootDir, ".terraform/modules/ignored/main.tf", `ignored`)

	tree := testTree(rootDir)
	plan, err := PlanStripping(context.Background(), tree, StripModeFull)
	require.NoError(t, err)

	assert.True(t, plan.IncludePath(filepath.Join(rootDir, "main.tf"), false))
	assert.True(t, plan.IncludePath(filepath.Join(rootDir, "README.md"), false))
	assert.True(t, plan.IncludePath(filepath.Join(rootDir, "examples", "example.tf"), false))
	assert.False(t, plan.IncludePath(filepath.Join(rootDir, ".terraform", "modules", "ignored", "main.tf"), false))
	assert.Equal(t, StripModeFull, plan.Mode)
	assert.Zero(t, plan.StrippedBytes)
}

func TestPlanStrippingKeepsResolvedModuleWhoseNameStartsWithTwoDots(t *testing.T) {
	rootDir := t.TempDir()
	moduleDir := filepath.Join(rootDir, "..generated")
	configPath := filepath.Join(moduleDir, "main.tf")
	writeFile(t, rootDir, "main.tf", `module "generated" { source = "./..generated" }`)
	writeFile(t, rootDir, "..generated/main.tf", `output "kept" { value = true }`)
	root := &ModuleNode{Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}
	child := &ModuleNode{Key: "generated", Name: "generated", InstallDir: moduleDir, PackageRoot: rootDir, IsLocal: true, Parent: root}
	root.Children = []*ModuleNode{child}
	tree := &ResolvedTree{Root: root, AllModules: []*ModuleNode{root, child}, Packages: map[string]*DownloadedPackage{}}

	for _, mode := range []StripMode{StripModeOptimistic, StripModeAggressive} {
		t.Run(string(mode), func(t *testing.T) {
			plan, err := PlanStripping(context.Background(), tree, mode)
			require.NoError(t, err)
			assert.True(t, plan.IncludePath(configPath, false), "an in-package '..name' is not parent traversal")
		})
	}
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

	plan, err := PlanStripping(context.Background(), tree, StripModeOptimistic)
	require.NoError(t, err)

	assert.True(t, plan.IncludePath(filepath.Join(packageRoot, "modules", "app", "main.tf"), false))
	assert.True(t, plan.IncludePath(filepath.Join(packageRoot, "modules", "worker", "main.tf"), false))
	assert.True(t, plan.IncludePath(filepath.Join(packageRoot, "shared", "templates", "app.tftpl"), false))
	assert.False(t, plan.IncludePath(filepath.Join(packageRoot, "README.md"), false))
	assert.False(t, plan.IncludePath(filepath.Join(packageRoot, "examples", "example.tf"), false))
	assert.Greater(t, plan.StrippedFiles, 0)
	assert.Greater(t, plan.StrippedBytes, int64(0))
}

// TestOptimistic_DynamicReadKeepsOwningModuleSubtree pins ADR-0001's
// owning-package scope (the scope-bug fix): a dynamic file() in a SUBJECT
// module under //subdir escalates IncludeAll to the module's OWN (narrowest)
// owning package — its directory subtree (modules/app) — NOT the broadest
// ancestor (packageRoot). So the entry module subtree is kept verbatim, while
// a package-level README and an unrelated shared/ sibling at the broadest
// ancestor are trimmed. (The earlier module-dir default kept README via the
// broadest-ancestor fallback bug; ADR-0001 "Owning package" makes that wrong.)
func TestOptimistic_DynamicReadKeepsOwningModuleSubtree(t *testing.T) {
	packageRoot := t.TempDir()
	writeFile(t, packageRoot, "modules/app/main.tf", `variable "name" { type = string }
locals { rendered = file("../shared/${var.name}.txt") }`)
	writeFile(t, packageRoot, "modules/app/runtime.txt", `kept — inside app's owning package subtree`)
	writeFile(t, packageRoot, "shared/a.txt", `stripped — outside app subtree, ownable package not reachable`)
	writeFile(t, packageRoot, "README.md", `stripped — narrow scope no longer leaks to packageRoot`)

	root := &ModuleNode{Key: "", Name: "root", InstallDir: filepath.Join(packageRoot, "modules", "app"), PackageRoot: packageRoot, IsLocal: true}
	tree := &ResolvedTree{Root: root, AllModules: []*ModuleNode{root}, Packages: map[string]*DownloadedPackage{}}

	plan, err := PlanStripping(context.Background(), tree, StripModeOptimistic)
	require.NoError(t, err)

	// Entry module subtree (the narrow owning package) is kept verbatim.
	assert.True(t, plan.IncludePath(filepath.Join(packageRoot, "modules", "app", "main.tf"), false))
	assert.True(t, plan.IncludePath(filepath.Join(packageRoot, "modules", "app", "runtime.txt"), false),
		"dynamic read should keep the module's own owning package (subtree) verbatim")
	// Broadest-ancestor (packageRoot) content is NOT kept — scope-bug fix (ADR-0001).
	assert.False(t, plan.IncludePath(filepath.Join(packageRoot, "README.md"), false),
		"narrowest-owning-package must no longer leak IncludeAll to the package root")
	assert.False(t, plan.IncludePath(filepath.Join(packageRoot, "shared", "a.txt"), false))
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

	plan, err := PlanStripping(context.Background(), tree, StripModeAggressive)
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
	plan, err := PlanStripping(context.Background(), tree, StripModeOptimistic)
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

	plan, err := PlanStripping(context.Background(), tree, StripModeOptimistic)
	require.NoError(t, err)

	var found bool
	var foundWarning *StripWarning
	for i := range plan.Warnings {
		w := &plan.Warnings[i]
		if strings.Contains(w.Message, "modules") && strings.Contains(w.Message, "pkg-a.git") &&
			strings.Contains(w.Message, "review item 4") {
			found = true
			foundWarning = w
			break
		}
	}
	assert.True(t, found,
		"item 4: expected a warning about the excluded 'modules' sibling in remote package pkg-a.git, got %+v",
		plan.Warnings)
	// Review nit #1: StripWarning.ModuleKey must be populated with the package's
	// owning module key so a metadata consumer can link the warning back to the
	// implicated module (the field was previously declared but never set).
	if assert.NotNil(t, foundWarning, "expected the excluded-sibling warning to be present") {
		assert.Contains(t, foundWarning.ModuleKey, "ext",
			"ModuleKey must name the owning module of the excluded-sibling package")
		assert.NotEmpty(t, foundWarning.ModuleKey,
			"ModuleKey must not be empty for a package-scoped warning")
	}
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

// TestOptimistic_KeepsPathModuleReferencedSubtree (ADR-0001 Ex 1) verifies that
// a plain attribute source_path = "${path.module}/src/hello-world" — NOT a
// file() call — is detected as risk signal #2, escalating the module's whole
// owning package to verbatim. The lambda src/ subtree (no .tf in src/) is kept
// under optimistic, even though no module block references it. Under
// aggressive the src/ file is dropped and a filesystem-read warning is emitted.
func TestOptimistic_KeepsPathModuleReferencedSubtree(t *testing.T) {
	rootDir := t.TempDir()
	writeFile(t, rootDir, "main.tf", `
module "hello-world-lambda" {
  source     = "terraform-aws-modules/lambda/aws"
  source_path = "${path.module}/src/hello-world"
}
`)
	writeFile(t, rootDir, "src/hello-world/main.go", `package main`)

	root := &ModuleNode{Key: "", Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}
	tree := &ResolvedTree{Root: root, AllModules: []*ModuleNode{root}, Packages: map[string]*DownloadedPackage{}}

	t.Run("optimistic keeps path.module-referenced subtree", func(t *testing.T) {
		plan, err := PlanStripping(context.Background(), tree, StripModeOptimistic)
		require.NoError(t, err)
		assert.True(t, plan.IncludePath(filepath.Join(rootDir, "main.tf"), false), "entry .tf config kept")
		assert.True(t, plan.IncludePath(filepath.Join(rootDir, "src", "hello-world", "main.go"), false),
			"risk signal #2 must keep the owning package (module subtree) verbatim")
		// The risk signal surfaces in metadata filesystem functions.
		var foundPathModule bool
		for _, ref := range plan.FilesystemFunctions {
			if ref.Function == pathModuleFunction && ref.Handling == handlingDynamicFallback {
				foundPathModule = true
				break
			}
		}
		assert.True(t, foundPathModule, "expected a path.module risk-signal ref, got %+v", plan.FilesystemFunctions)
	})

	t.Run("aggressive drops it with a warning (ADR-0001 Ex 4)", func(t *testing.T) {
		plan, err := PlanStripping(context.Background(), tree, StripModeAggressive)
		require.NoError(t, err)
		assert.True(t, plan.IncludePath(filepath.Join(rootDir, "main.tf"), false), ".tf config kept under aggressive")
		assert.False(t, plan.IncludePath(filepath.Join(rootDir, "src", "hello-world", "main.go"), false),
			"aggressive trims even risk-signaled non-.tf content")
		require.NotEmpty(t, plan.Warnings, "aggressive must warn on detected filesystem reads")
		assert.Contains(t, plan.Warnings[0].Message, "filesystem reads were detected")
	})
}

// TestOptimistic_PathModuleInProvisionerKeepsWholeOwningPackage (ADR-0001 Ex 3)
// verifies a null_resource local-exec interpreter/command containing
// "${path.module}/package.py" escalates the whole owning package — package.py
// AND an unreferenced sibling in the same module dir survive (whole-package,
// not just the named file), because package.py may read arbitrary siblings.
func TestOptimistic_PathModuleInProvisionerKeepsWholeOwningPackage(t *testing.T) {
	rootDir := t.TempDir()
	writeFile(t, rootDir, "main.tf", `
resource "null_resource" "archive" {
  provisioner "local-exec" {
    interpreter = [local.python, "${path.module}/package.py", "build"]
    command     = "echo plan"
  }
}
locals { python = "python3" }
`)
	writeFile(t, rootDir, "package.py", `print("build")`)
	writeFile(t, rootDir, "sibling_unreferenced.txt", `package.py may read me at runtime`)

	root := &ModuleNode{Key: "", Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}
	tree := &ResolvedTree{Root: root, AllModules: []*ModuleNode{root}, Packages: map[string]*DownloadedPackage{}}

	plan, err := PlanStripping(context.Background(), tree, StripModeOptimistic)
	require.NoError(t, err)
	assert.True(t, plan.IncludePath(filepath.Join(rootDir, "package.py"), false),
		"package.py referenced via ${path.module}/ must be kept")
	assert.True(t, plan.IncludePath(filepath.Join(rootDir, "sibling_unreferenced.txt"), false),
		"whole owning package kept verbatim — unreferenced siblings survive (ADR-0001 Ex 3)")
}

// TestOptimistic_OwningPackageScopedNotAncestor (ADR-0001 scope-bug regression)
// verifies a risk signal in a VENDORED module (staged under the subject's
// _vendor/) escalates IncludeAll on the VENDOR's package plan, NOT the user's
// root package. A sentinel file in the user root (in a .tf-less dir, so the
// anchor rule cannot rescue it) must be STRIPPED — proving the dynamic fallback
// no longer leaks to the broadest ancestor (the user repo root).
func TestOptimistic_OwningPackageScopedNotAncestor(t *testing.T) {
	rootDir := t.TempDir()
	writeFile(t, rootDir, "main.tf", `module "ext" { source = "git::https://example.com/pkg.git" }`)
	// Sentinel in a .tf-less dir inside the user root: not an anchor, not
	// referenced. It is kept ONLY if the dynamic fallback leaks to the user root.
	writeFile(t, rootDir, "docs/secret.txt", `must be stripped — narrow scope`)

	// Simulate the vendored package staged under the subject's _vendor/ tree
	// (the layout the bundler produces). Its config carries a ${path.module}/
	// risk signal (#2) in a PLAIN attribute (the headliner class).
	vendorRoot := filepath.Join(rootDir, "_vendor", "pkg")
	writeFile(t, vendorRoot, "main.tf", `
locals {
  build = "${path.module}/build.sh"
}
output "x" { value = local.build }
`)
	writeFile(t, vendorRoot, "examples/witha.tf", `output "a" { value = true }`)
	writeFile(t, vendorRoot, "build.sh", `echo build`)

	root := &ModuleNode{Key: "", Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}
	ext := &ModuleNode{
		Key:         "ext",
		Name:        "ext",
		Parent:      root,
		InstallDir:  vendorRoot,
		PackageRoot: vendorRoot,
		IsRemote:    true,
		Source:      ModuleSource{Raw: "git::https://example.com/pkg.git", PackageAddr: "git::https://example.com/pkg.git", Type: SourceGit},
	}
	root.Children = []*ModuleNode{ext}
	tree := &ResolvedTree{
		Root:       root,
		AllModules: []*ModuleNode{root, ext},
		VendorDir:  "_vendor",
		Packages: map[string]*DownloadedPackage{
			"git::https://example.com/pkg.git": {
				PackageAddr: "git::https://example.com/pkg.git",
				LocalDir:    vendorRoot,
			},
		},
	}

	plan, err := PlanStripping(context.Background(), tree, StripModeOptimistic)
	require.NoError(t, err)

	// Vendor's own files kept via its risk signal (#2) scoping to the vendor plan.
	assert.True(t, plan.IncludePath(filepath.Join(vendorRoot, "main.tf"), false), "vendor entry .tf config kept")
	assert.True(t, plan.IncludePath(filepath.Join(vendorRoot, "build.sh"), false),
		"vendor ${path.module}/build.sh kept via whole-owning-package on the VENDOR plan")

	// User-root sentinel STRIPPED — the scope bug (broadest-ancestor fallback)
	// would have kept it by setting IncludeAll on the user root.
	assert.False(t, plan.IncludePath(filepath.Join(rootDir, "docs", "secret.txt"), false),
		"vendor risk signal must NOT leak IncludeAll to the user root (scope-bug fix)")

	// Vendor's unreferenced examples/witha.tf: not an anchor (vendor carveout),
	// and the risk signal escalates the vendor PACKAGE (LocalDir = vendorRoot),
	// which DOES include vendor examples/ as verbatim siblings of vendor main.
	// Per ADR-0001, the whole owning downloaded package is kept when risky.
	assert.True(t, plan.IncludePath(filepath.Join(vendorRoot, "examples", "witha.tf"), false),
		"vendored risk-flagged package is kept verbatim, including its examples/")
}

// TestAnchor_SubjectKeepsTerraformDirs VendorDoesNot (ADR-0001 Ex 2) verifies
// the anchor rule: an UNREFERENCED .tf-bearing directory under the press subject
// is preserved at its path with its .tf config (consumers can // it), while a
// .tf-bearing directory inside the vendor directory is NEVER an anchor and is
// trimmed unless the vendor package carries a risk signal.
func TestAnchor_SubjectKeepsTerraformDirsVendorDoesNot(t *testing.T) {
	rootDir := t.TempDir()
	writeFile(t, rootDir, "main.tf", `output "entry" { value = "ok" }`)
	// Unreferenced .tf-dir WITHIN the subject -> anchor.
	writeFile(t, rootDir, "examples/big-example/main.tf", `output "big" { value = true }`)
	writeFile(t, rootDir, "examples/big-example/README.md", `non-.tf in anchor dir — trimmed unless risk/static`)

	// Vendored package (no risk signal) with a .tf-bearing examples/ dir.
	vendorRoot := filepath.Join(rootDir, "_vendor", "foo")
	writeFile(t, vendorRoot, "main.tf", `output "v" { value = 1 }`)
	writeFile(t, vendorRoot, "examples/witha.tf", `output "a" { value = true }`)

	root := &ModuleNode{Key: "", Name: "root", InstallDir: rootDir, PackageRoot: rootDir, IsLocal: true}
	ext := &ModuleNode{
		Key:         "ext",
		Name:        "ext",
		Parent:      root,
		InstallDir:  vendorRoot,
		PackageRoot: vendorRoot,
		IsRemote:    true,
		Source:      ModuleSource{Raw: "git::https://example.com/foo.git", PackageAddr: "git::https://example.com/foo.git", Type: SourceGit},
	}
	root.Children = []*ModuleNode{ext}
	tree := &ResolvedTree{
		Root:       root,
		AllModules: []*ModuleNode{root, ext},
		VendorDir:  "_vendor",
		Packages: map[string]*DownloadedPackage{
			"git::https://example.com/foo.git": {
				PackageAddr: "git::https://example.com/foo.git",
				LocalDir:    vendorRoot,
			},
		},
	}

	plan, err := PlanStripping(context.Background(), tree, StripModeOptimistic)
	require.NoError(t, err)

	// Subject anchor: unreferenced .tf-dir preserved at path + .tf config kept.
	assert.True(t, plan.IncludePath(filepath.Join(rootDir, "examples", "big-example"), true),
		"anchor dir within subject preserved at its staged path")
	assert.True(t, plan.IncludePath(filepath.Join(rootDir, "examples", "big-example", "main.tf"), false),
		"anchor .tf config kept (ADR-0001 Ex 2 — //examples/big-example works)")
	// Non-.tf file in the anchor dir: NOT kept wholesale (corrected anchor reading).
	assert.False(t, plan.IncludePath(filepath.Join(rootDir, "examples", "big-example", "README.md"), false),
		"non-.tf in anchor dir follows the keep-set — trimmed without a match/risk signal")

	// Vendor carveout: vendor's .tf-dir is NOT an anchor; without a risk signal
	// its unreferenced examples/ dir is trimmed.
	assert.False(t, plan.IncludePath(filepath.Join(vendorRoot, "examples", "witha.tf"), false),
		"vendor dirs are never anchors (ADR-0001) — unreferenced vendor examples trimmed")
	// Vendor entry .tf config is still kept (config preservation).
	assert.True(t, plan.IncludePath(filepath.Join(vendorRoot, "main.tf"), false),
		"vendor entry .tf config kept so consumers can validate")
}
