//go:build integration

//nolint:gosec // integration tests use tempdirs, local git repos, httptest servers, and subprocess calls
package tofupress

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ulikunitz/xz"
)

// TestAcceptanceConsumeMatrix is the regression gate for review items 1, 2, and 3.
//
// It drives the FULL produce → distribute → consume path for every cell of
//
//	source-type ∈ {local-flat, local-monorepo-//, registry, git, http-archive}
//	× format ∈ {zip, tar.gz, tar.xz}
//	× {default, --oci-compliant}
//
// For each cell: build the real tofupress CLI binary → bundle the fixture →
// extract the archive → `tofu init -backend=false -input=false` → `tofu validate`
// at the unpacked archive root. Skips when `tofu` is not on PATH and when
// --oci-compliant is combined with a non-zip format (the bundler rejects that).
//
// Local-only network substitutes (testRegistryBaseURL is for in-process tests;
// here we drive the CLI binary through the TOFUPRESS_REGISTRY_BASE_URL env var
// added alongside this test). Real remote registries and GitHub are NEVER hit.
//
// Cells deliberately fail before items 1 and 2 are fixed:
//   - registry cells fail with `Invalid registry module source address` because the
//     rewritten local `source` block still carries `version = "x.y.z"` (item 1);
//   - local-monorepo-// cells fail with "OpenTofu initialized in an empty
//     directory!" because the entry module is buried in a subdirectory of the
//     archive and `tofu init` at the archive root finds no configuration files
//     (item 2).
//
// Both failures are the SilentSuccess gaps item 3 exists to make un-shippable.
func TestAcceptanceConsumeMatrix(t *testing.T) {
	skipIfNoTofu(t)
	bin := buildTofuPressBinary(t)

	sources := []string{"local-flat", "local-monorepo", "registry", "git", "http-archive"}
	formats := []string{"zip", "tar.gz", "tar.xz"}

	for _, srcKind := range sources {
		for _, format := range formats {
			for _, ci := range []bool{false, true} {
				cell := matrixCell{sourceKind: srcKind, format: format, ociCompliant: ci}
				t.Run(cell.name(), func(t *testing.T) {
					runMatrixCell(t, bin, cell)
				})
			}
		}
	}
}

type matrixCell struct {
	sourceKind   string
	format       string
	ociCompliant bool
}

func (c matrixCell) name() string {
	oci := "default"
	if c.ociCompliant {
		oci = "oci"
	}
	return fmt.Sprintf("%s__%s__%s", c.sourceKind, c.format, oci)
}

type cellFixture struct {
	source    string   // source arg passed to `tofupress module`
	bundleEnv []string // extra env for the bundle subprocess (e.g. registry stub URL)
	topModule string   // the top-level module name declared in the consumer's main.tf at archive root
}

func runMatrixCell(t *testing.T, bin string, cell matrixCell) {
	t.Helper()

	if cell.ociCompliant && cell.format != "zip" {
		t.Skipf("--oci-compliant requires zip format (got %s)", cell.format)
	}

	fix := prepareCellFixture(t, cell)
	artifactPath := filepath.Join(t.TempDir(), "bundle."+cell.format)
	args := []string{"module", fix.source, artifactPath, "--format=" + cell.format}
	if cell.ociCompliant {
		args = append(args, "--oci-compliant")
	}
	runModuleCmd(t, bin, fix.bundleEnv, args)

	extractDir := t.TempDir()
	require.NoError(t, matrixExtractArchive(t, artifactPath, cell.format, extractDir),
		"failed to extract %s", artifactPath)

	// Item 2 SilentSuccess gate: a consumable bundle MUST ship the entry module
	// at the archive root, otherwise `tofu init` at the root silently passes an
	// empty directory and validate passes too.
	require.FileExists(t, filepath.Join(extractDir, "main.tf"),
		"item 2 gate: archive root must contain main.tf for the consumer to tofu init at the unpacked root")

	// tofu init + validate at the unpacked archive root, no `cd`, no edits.
	tool := iacTool{Name: "tofu"}
	initOut := runIACToolCombined(t, tool, extractDir, "init", "-backend=false", "-input=false", "-no-color")
	assertTofuInitNonEmpty(t, initOut)
	runIACTool(t, tool, extractDir, "validate", "-no-color")

	// Item 1 SilentSuccess gate: verify the rewritten source block no longer
	// carries a stray `version =` on a registry→local rewrite. We check by
	// parsing the bundle's main.tf module block whose name matches the entry.
	if cell.sourceKind == "registry" {
		registryRewriteGate(t, extractDir, fix.topModule)
	}
}

func skipIfNoTofu(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tofu"); err != nil {
		t.Skipf("tofu not on PATH: %v", err)
	}
}

func runModuleCmd(t *testing.T, bin string, extraEnv []string, args []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = projectRoot(t)
	env := append(os.Environ(), "TF_IN_AUTOMATION=1")
	env = append(env, extraEnv...)
	cmd.Env = env

	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("tofupress %s failed: %v\nstdout:\n%s\nstderr:\n%s",
			strings.Join(args, " "), err, out.String(), errOut.String())
	}
}

func runIACToolCombined(t *testing.T, tool iacTool, dir string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, tool.Name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), toolEnv(dir, tool)...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out // combine for assertion
	require.NoError(t, cmd.Run(),
		"%s %s failed in %s:\n%s", tool.Name, strings.Join(args, " "), dir, out.String())
	return out.String()
}

// assertTofuInitNonEmpty rejects the silent-empty-init failure mode item 2
// produces: when the archive root has no .tf files, `tofu init` still exits 0
// but prints "initialized in an empty directory!".
func assertTofuInitNonEmpty(t *testing.T, initOut string) {
	t.Helper()
	assert.Contains(t, initOut, "successfully initialized",
		"tofu init did not report success; output:\n%s", initOut)
	assert.NotContains(t, initOut, "in an empty directory",
		"item 2 SilentSuccess: tofu initialized an empty directory (entry module not at archive root); output:\n%s", initOut)
}

// registryRewriteGate asserts that the registry module block in the bundle's
// main.tf has its `version` attribute removed (item 1) and the source rewritten
// to a local ./_vendor/... path.
func registryRewriteGate(t *testing.T, extractDir, topModule string) {
	t.Helper()
	blocks, err := ExtractModuleBlocks(filepath.Join(extractDir, "main.tf"))
	require.NoError(t, err)
	var matched *ModuleBlock
	for i := range blocks {
		if blocks[i].Name == topModule {
			matched = &blocks[i]
			break
		}
	}
	require.NotNil(t, matched, "registry cell: top-level module %q not found in archive main.tf", topModule)
	assert.True(t, strings.HasPrefix(matched.Source, "./"+defaultVendorDir+"/") || strings.Contains(matched.Source, "/"+defaultVendorDir+"/"),
		"registry cell: source %q should point at a vendored local path", matched.Source)
	assert.Empty(t, matched.Version,
		"item 1 regression: registry module %q still carries version=%q after rewrite to local source", topModule, matched.Version)
}

// ---------------------------------------------------------------------------
// Fixture preparation per source kind
// ---------------------------------------------------------------------------

func prepareCellFixture(t *testing.T, cell matrixCell) cellFixture {
	t.Helper()
	switch cell.sourceKind {
	case "local-flat":
		return setupLocalFlat(t)
	case "local-monorepo":
		return setupLocalMonorepo(t)
	case "registry":
		return setupRegistry(t)
	case "git":
		return setupGitSource(t)
	case "http-archive":
		return setupHTTPArchive(t)
	default:
		t.Fatalf("unknown source kind %q", cell.sourceKind)
		return cellFixture{}
	}
}

// setupLocalFlat: a tempdir root with one local sibling module. No remote deps.
//
//	root/main.tf -> module "leaf" { source = "./mod_net" }
//	root/mod_net/main.tf -> output "marker" { value = "leaf" }
func setupLocalFlat(t *testing.T) cellFixture {
	t.Helper()
	root := t.TempDir()
	writeIntegrationFile(t, root, "main.tf",
		`module "leaf" {
  source = "./mod_net"
}
`)
	writeIntegrationFile(t, root, "mod_net/main.tf",
		`output "marker" { value = "leaf" }`)
	return cellFixture{source: root, topModule: "leaf"}
}

// setupLocalMonorepo: a tempdir monorepo with the entry at <repo>//environments/prod
// referencing a sibling module via ../../modules/network.
//
//	<repo>/environments/prod/main.tf -> module "network" { source = "../../modules/network" }
//	<repo>/modules/network/main.tf -> output "marker" { value = "network" }
//
// Pressed via `tofupress module <repo>//environments/prod out.<fmt>`.
func setupLocalMonorepo(t *testing.T) cellFixture {
	t.Helper()
	repo := t.TempDir()
	writeIntegrationFile(t, repo, "environments/prod/main.tf",
		`module "network" {
  source = "../../modules/network"
}
`)
	writeIntegrationFile(t, repo, "modules/network/main.tf",
		`output "marker" { value = "network" }`)
	return cellFixture{source: repo + "//environments/prod", topModule: "network"}
}

// setupRegistry: a local httptest server stubbing the registry API to return
// a git::file:// URL of a local git repo containing the module. The CLI binary
// is driven at this stub via TOFUPRESS_REGISTRY_BASE_URL so the registry cell
// never hits the network. The root fixture declares:
//
//	module "vpc" { source = "acme/vpc/aws"; version = "1.0.0" }
//
// Item 1 strip applies on rewrite to ./_vendor/...
func setupRegistry(t *testing.T) cellFixture {
	t.Helper()
	// Local git repo holding the registry module body.
	modRepo := t.TempDir()
	writeIntegrationFile(t, modRepo, "main.tf",
		`output "marker" { value = "vpc" }`)
	commitLocalRepo(t, modRepo, "registry-module")

	// Registry API stub: returns the registryResponse pointing at the local git repo.
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w,
			`{"source": "file://%s", "version": "1.0.0", "tag": "v1.0.0"}`,
			modRepo)
	}))
	t.Cleanup(stub.Close)

	root := t.TempDir()
	writeIntegrationFile(t, root, "main.tf",
		`module "vpc" {
  source  = "acme/vpc/aws"
  version = "1.0.0"
}
`)
	return cellFixture{
		source:    root,
		bundleEnv: []string{"TOFUPRESS_REGISTRY_BASE_URL=" + stub.URL},
		topModule: "vpc",
	}
}

// setupGitSource: a tempdir root whose main.tf references a local git repo via
// git::file://...?ref=v1.0.0. Mirrors the existing cross-repo test topology at
// the matrix's git cell.
//
//	root/main.tf -> module "ext" { source = "git::file://<repo>?ref=v1.0.0" }
//	<repo>/main.tf -> output "marker" { value = "gitext" }
func setupGitSource(t *testing.T) cellFixture {
	t.Helper()
	modRepo := t.TempDir()
	writeIntegrationFile(t, modRepo, "main.tf",
		`output "marker" { value = "gitext" }`)
	commitLocalRepo(t, modRepo, "git-dep")

	root := t.TempDir()
	source := gitTaggedSource(modRepo, "v1.0.0")
	writeIntegrationFile(t, root, "main.tf",
		fmt.Sprintf(`module "ext" {
  source = %q
}
`, source))
	return cellFixture{source: root, topModule: "ext"}
}

// setupHTTPArchive: a tempdir root whose main.tf references an HTTP-served
// tar.gz module archive served by a local httptest server. Mirrors fetcher_test
// fixtures but at the consume-matrix level.
//
//	root/main.tf -> module "httpmod" { source = "<httptest URL>/mod.tar.gz" }
//	<served mod.tar.gz> -> mod/main.tf -> output "marker" { value = "httpmod" }
func setupHTTPArchive(t *testing.T) cellFixture {
	t.Helper()
	// Build the module directory then tar.gz it for the server to serve.
	modDir := t.TempDir()
	writeIntegrationFile(t, modDir, "main.tf",
		`output "marker" { value = "httpmod" }`)

	archivePath := filepath.Join(t.TempDir(), "mod.tar.gz")
	require.NoError(t, tarGzDir(modDir, archivePath))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		http.ServeFile(w, r, archivePath) //nolint:gosec // test fixture path
	}))
	t.Cleanup(server.Close)

	root := t.TempDir()
	writeIntegrationFile(t, root, "main.tf",
		fmt.Sprintf(`module "httpmod" {
  source = "%s/mod.tar.gz"
}
`, server.URL))
	return cellFixture{source: root, topModule: "httpmod"}
}

// commitLocalRepo runs the standard git init + initial commit + tag flow.
func commitLocalRepo(t *testing.T, dir, name string) {
	t.Helper()
	runCommand(t, dir, nil, "git", "init")
	runCommand(t, dir, nil, "git", "config", "user.email", "matrix@example.invalid")
	runCommand(t, dir, nil, "git", "config", "user.name", "TofuPress Matrix")
	runCommand(t, dir, nil, "git", "add", ".")
	runCommand(t, dir, nil, "git", "commit", "-m", "initial "+name)
	runCommand(t, dir, nil, "git", "tag", "v1.0.0")
}

// ---------------------------------------------------------------------------
// Archive extraction (zip / tar.gz / tar.xz) into a destination directory.
// ---------------------------------------------------------------------------

func matrixExtractArchive(t *testing.T, artifactPath, format, dst string) error {
	t.Helper()
	switch format {
	case "zip":
		return matrixExtractZip(artifactPath, dst)
	case "tar.gz":
		return matrixExtractTar(artifactPath, dst, true)
	case "tar.xz":
		return matrixExtractTar(artifactPath, dst, false)
	default:
		return fmt.Errorf("unsupported format %q", format)
	}
}

func matrixExtractZip(src, dst string) error {
	r, err := zip.OpenReader(src) //nolint:gosec // test fixture path
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		if err := matrixExtractZipFile(f, dst); err != nil {
			return err
		}
	}
	return nil
}

func matrixExtractZipFile(f *zip.File, dst string) error {
	path := filepath.Join(dst, f.Name) //nolint:gosec // test destination
	if f.FileInfo().IsDir() {
		return os.MkdirAll(path, 0o755)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, rc)
	return err
}

func matrixExtractTar(src, dst string, gz bool) error {
	f, err := os.Open(src) //nolint:gosec // test fixture path
	if err != nil {
		return err
	}
	defer f.Close()
	var tr *tar.Reader
	if gz {
		gzr, gzErr := gzip.NewReader(f)
		if gzErr != nil {
			return gzErr
		}
		defer gzr.Close()
		tr = tar.NewReader(gzr)
	} else {
		xzr, xzErr := xz.NewReader(f)
		if xzErr != nil {
			return xzErr
		}
		tr = tar.NewReader(xzr)
	}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dst, hdr.Name) //nolint:gosec // test destination
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(hdr.Mode&0o755))
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		case tar.TypeSymlink:
			_ = os.Symlink(hdr.Linkname, target) // best effort; symlinks not load-bearing for the matrix
		}
	}
}

// tarGzDir writes a tar.gz of a single top-level directory entry `mod/`
// containing each file in src, so it serves as a module archive for fetcher.
func tarGzDir(src, dst string) error {
	out, err := os.Create(dst) //nolint:gosec // path provided by caller
	if err != nil {
		return err
	}
	defer out.Close()
	gzw := gzip.NewWriter(out)
	defer gzw.Close()
	tw := tar.NewWriter(gzw)
	defer tw.Close()

	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		// Serve a single top-level "mod/" directory so fetcher extracts to mod/.
		archiveName := filepath.ToSlash(filepath.Join("mod", rel))
		hdr := &tar.Header{
			Name:     archiveName,
			Mode:     int64(info.Mode()),
			ModTime:  info.ModTime(),
			Size:     info.Size(),
			Typeflag: tar.TypeReg,
		}
		if info.IsDir() {
			hdr.Typeflag = tar.TypeDir
			return tw.WriteHeader(hdr)
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(path) //nolint:gosec // archive source path
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
}
