//go:build integration

//nolint:gosec // integration tests execute test binary and access local paths
package tofupress

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAcceptance_Strip_RiskSignal2_PathModuleSurvivesBundle (ADR-0001 Ex 1, e2e)
// is the end-to-end pinning of the headliner: a real repo whose main.tf
// references a module-relative file via a PLAIN attribute / provisioner command
// (NOT a file() call) — signal #2 — must have that file survive the bundle
// under the optimistic default. This exercises the full pipeline (resolve →
// stage → strip → archive), not just PlanStripping, so a regression in
// staging, pivot re-planning, or aggregation cannot silently re-break the
// ADR's motivation with all unit tests green.
func TestAcceptance_Strip_RiskSignal2_PathModuleSurvivesBundle(t *testing.T) {
	bin := buildBinary(t)
	src := t.TempDir()
	// A null_resource local-exec command referencing ${path.module}/src/... —
	// the exact class the old module-dir default silently stripped. A plain
	// local attribute carries the same shape to exercise attribute detection.
	writeIntegrationFile(t, src, "main.tf", `
locals {
  source_path = "${path.module}/src/hello-world"
}

resource "null_resource" "go_lambda_build" {
  provisioner "local-exec" {
    command = "cd ${path.module}/src/hello-world && go build -o bootstrap main.go"
  }
}

output "source_path" { value = local.source_path }
`)
	writeIntegrationFile(t, src, "src/hello-world/main.go", `package main

import "fmt"

func main() { fmt.Println("hello") }
`)
	writeIntegrationFile(t, src, "src/hello-world/go.mod", "module hello-world\n\ngo 1.26\n")

	t.Run("optimistic default keeps src/ verbatim", func(t *testing.T) {
		artifact := filepath.Join(t.TempDir(), "bundle.zip")
		runTofuPressBundle(t, bin, src, artifact, "--format=zip")

		// Assert the headliner: src/hello-world/main.go survives, not stripped.
		names := zipEntryPaths(t, artifact)
		assert.Contains(t, names, "src/hello-world/main.go",
			"ADR-0001 Ex 1: ${path.module}/src/hello-world must keep src/.../main.go under optimistic")
		assert.Contains(t, names, "src/hello-world/go.mod",
			"whole owning package (module subtree) kept verbatim — siblings survive")

		m := metadataFromArtifact(t, artifact)
		assert.Equal(t, string(StripModeOptimistic), m.Command.Options.StripMode)
		// The path.module risk signal surfaces in metadata.
		var foundPathModule bool
		for _, ref := range m.FilesystemFunctions {
			if ref.Kind == refKindPathTemplateRisk && ref.Function == pathModuleFunction {
				foundPathModule = true
				break
			}
		}
		assert.True(t, foundPathModule, "expected a path.module risk-signal ref in metadata, got %+v", m.FilesystemFunctions)
	})

	t.Run("aggressive drops src with a warning (ADR-0001 Ex 4)", func(t *testing.T) {
		artifact := filepath.Join(t.TempDir(), "bundle-aggr.zip")
		_, stderr := runTofuPressBundle(t, bin, src, artifact, "--format=zip", "--strip=aggressive")

		names := zipEntryPaths(t, artifact)
		assert.Contains(t, names, "main.tf", ".tf config kept under aggressive")
		assert.NotContains(t, names, "src/hello-world/main.go",
			"aggressive trims even risk-signaled non-.tf content (Ex 4)")
		assert.Contains(t, stderr, "filesystem reads were detected",
			"aggressive must warn when risk signals are present")
	})
}

// TestAcceptance_Strip_RiskSignal2_ProvisionerKeepsPackageAndSibling (ADR-0001 Ex 3, e2e)
// pins the whole-owning-package escalation end-to-end: a null_resource
// provisioner interpreter containing "${path.module}/package.py" keeps
// package.py AND an unreferenced sibling in the same module dir (package.py
// may read arbitrary siblings at runtime).
func TestAcceptance_Strip_RiskSignal2_ProvisionerKeepsPackageAndSibling(t *testing.T) {
	bin := buildBinary(t)
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", `
locals { python = "python3" }

resource "null_resource" "archive" {
  provisioner "local-exec" {
    interpreter = [local.python, "${path.module}/package.py", "build"]
    command     = "echo plan"
  }
}

output "ok" { value = "ok" }
`)
	writeIntegrationFile(t, src, "package.py", `print("build")`)
	writeIntegrationFile(t, src, "sibling_unreferenced.txt", `package.py may read me at runtime`)

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, src, artifact, "--format=zip")

	names := zipEntryPaths(t, artifact)
	assert.Contains(t, names, "package.py",
		"interpreter ${path.module}/package.py must keep package.py (Ex 3)")
	assert.Contains(t, names, "sibling_unreferenced.txt",
		"whole owning package kept verbatim — unreferenced siblings survive (Ex 3)")
}

// TestAcceptance_Strip_ScopeBug_VendorRiskDoesNotLeakToUserRoot (ADR-0001 scope-bug, e2e)
// reproduces the over-inclusion regression end-to-end: a vendored remote module
// whose config contains a ${path.module}/... risk signal must escalate its OWN
// downloaded package (keeping the vendor's build.sh), NOT leak IncludeAll to
// the user repo root — so a sentinel file in a .tf-less dir of the user root
// (not an anchor, not referenced) is STRIPPED.
func TestAcceptance_Strip_ScopeBug_VendorRiskDoesNotLeakToUserRoot(t *testing.T) {
	bin := buildBinary(t)
	// Vendored remote module whose main.tf carries a ${path.module}/build.sh
	// risk signal (#2, in a plain local).
	vendorRepo := createLocalGitModuleRepo(t, "risk-vendor", map[string]string{
		"main.tf": `
locals {
  build = "${path.module}/build.sh"
}

output "built" { value = local.build }
`,
		"build.sh":      "echo build",
		"examples/a.tf": `output "a" { value = true }`,
	})

	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", strings.Join([]string{
		`module "ext" { source = %q }`,
		``,
		`output "ext" { value = module.ext.built }`,
	}, "\n"))
	// Rewrite main.tf with the actual git source via the helper.
	writeIntegrationFile(t, src, "main.tf", strings.Join([]string{
		`module "ext" { source = "` + gitFileSource(vendorRepo) + `" }`,
		``,
		`output "ext" { value = module.ext.built }`,
	}, "\n"))
	// User-root sentinel in a .tf-less dir: not an anchor, not referenced.
	writeIntegrationFile(t, src, "docs/secret.txt", "must be stripped — narrow scope")

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, src, artifact, "--format=zip")

	names := zipEntryPaths(t, artifact)

	// Vendor's risk signal keeps its own package (build.sh survives).
	assert.True(t, hasPathMatching(names, "/build.sh"),
		"vendor ${path.module}/build.sh kept via whole-owning-package on the VENDOR plan")

	// User-root sentinel STRIPPED — the scope-bug (broadest-ancestor fallback)
	// would have leaked IncludeAll to the user root and kept this.
	assert.False(t, hasPathMatching(names, "/docs/secret.txt"),
		"vendor risk signal must NOT leak IncludeAll to the user root (scope-bug fix)")
}

// zipEntryPaths extracts a zip archive's regular-file entry paths (slash-joined,
// no leading slash) into a sorted-ish slice. This is the surface a consumer
// sees when unpacking — the headliner assertion ("src/hello-world/main.go
// survives") is checked against it.
func zipEntryPaths(t *testing.T, artifactPath string) []string {
	t.Helper()
	reader, err := zip.OpenReader(artifactPath) //nolint:gosec // test artifact path is safe
	require.NoError(t, err)
	defer reader.Close() //nolint:errcheck // test cleanup

	var names []string
	for _, file := range reader.File {
		if file.FileInfo().IsDir() {
			continue
		}
		// Drain to ensure the entry is real (not a zero-byte placeholder).
		rc, err := file.Open()
		if err != nil {
			continue
		}
		_, _ = io.Copy(io.Discard, rc) //nolint:errcheck // test read
		rc.Close()                     //nolint:errcheck // test cleanup
		names = append(names, file.Name)
	}
	return names
}

// hasPathMatching reports whether any entry path ends with suffix (slash-normalized),
// so the assertion is robust to the vendor directory's namespaced layout
// (_vendor/<ns>/<name>/...) which may vary between staging passes.
func hasPathMatching(names []string, suffix string) bool {
	for _, n := range names {
		if strings.HasSuffix(filepath.ToSlash(n), suffix) {
			return true
		}
	}
	return false
}

// Ensure os is referenced when the build tag is active (helper file compiles
// standalone under -tags=integration).
var _ = os.Stat
