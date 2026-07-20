//go:build integration

//nolint:gosec // integration tests use temporary directories and safe paths
package tofupress

import (
	"context"
	"fmt"
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
)

type iacTool struct {
	Name string
}

func TestIntegrationHelpersRequireTools(t *testing.T) {
	tools := requireAllIACTools(t)
	require.Len(t, tools, 2)
	assert.Equal(t, "terraform", tools[0].Name)
	assert.Equal(t, "tofu", tools[1].Name)
}

func requireIACTool(t *testing.T, name string) iacTool {
	t.Helper()
	path, err := exec.LookPath(name)
	require.NoError(t, err, "required integration tool %q is missing; run mise install", name)
	require.NotEmpty(t, path)
	return iacTool{Name: name}
}

func requireAllIACTools(t *testing.T) []iacTool {
	t.Helper()
	return []iacTool{
		requireIACTool(t, "terraform"),
		requireIACTool(t, "tofu"),
	}
}

func runCommand(t *testing.T, dir string, env []string, name string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s %s failed in %s:\n%s", name, strings.Join(args, " "), dir, string(output))
	return string(output)
}

func runCommandError(t *testing.T, dir string, env []string, name string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	output, err := cmd.CombinedOutput()
	require.Error(t, err, "%s %s unexpectedly succeeded in %s:\n%s", name, strings.Join(args, " "), dir, string(output))
	return string(output)
}

func toolEnv(dir string, tool iacTool) []string {
	return []string{
		"TF_IN_AUTOMATION=1",
		"TF_INPUT=0",
		"CHECKPOINT_DISABLE=1",
		"TF_DATA_DIR=" + filepath.Join(dir, ".terraform-"+tool.Name),
	}
}

func runIACTool(t *testing.T, tool iacTool, dir string, args ...string) string {
	t.Helper()
	return runCommand(t, dir, toolEnv(dir, tool), tool.Name, args...)
}

func runIACToolError(t *testing.T, tool iacTool, dir string, args ...string) string {
	t.Helper()
	return runCommandError(t, dir, toolEnv(dir, tool), tool.Name, args...)
}

func projectRoot(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	require.NoError(t, err)
	return filepath.Clean(filepath.Join(cwd, "..", ".."))
}

func buildTofuPressBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "tofupress")
	if strings.Contains(strings.ToLower(os.Getenv("GOOS")), "windows") {
		bin += ".exe"
	}
	runCommand(t, projectRoot(t), nil, "go", "build", "-o", bin, "./cmd/tofupress")
	return bin
}

func runTofuPressBundle(t *testing.T, bin, source, output string, flags ...string) (stdout string, stderr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	args := append([]string{"module", source, output}, flags...)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = projectRoot(t)
	cmd.Env = append(os.Environ(), "TF_IN_AUTOMATION=1")
	var outBuilder strings.Builder
	var errBuilder strings.Builder
	cmd.Stdout = &outBuilder
	cmd.Stderr = &errBuilder
	err := cmd.Run()
	require.NoError(t, err, "tofupress %s failed\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), outBuilder.String(), errBuilder.String())
	return outBuilder.String(), errBuilder.String()
}

func serveArtifact(t *testing.T, artifactPath string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, artifactPath)
	}))
	t.Cleanup(server.Close)
	return server.URL + "/" + filepath.Base(artifactPath)
}

func writeIntegrationFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func writeConsumerModule(t *testing.T, dir, source string) {
	t.Helper()
	writeIntegrationFile(t, dir, "main.tf", fmt.Sprintf(`
module "sut" {
  source = %q
}
`, source))
}

func metadataFromArtifact(t *testing.T, artifactPath string) *ArtifactMetadata {
	t.Helper()
	metadata, err := ReadMetadataFromArtifact(artifactPath)
	require.NoError(t, err)
	return metadata
}

func validateArchiveWithAllTools(t *testing.T, sourceURL string) {
	t.Helper()
	for _, tool := range requireAllIACTools(t) {
		validateArchiveWithTool(t, tool, sourceURL)
	}
}

func validateArchiveWithTool(t *testing.T, tool iacTool, sourceURL string) {
	t.Helper()
	consumerDir := t.TempDir()
	writeConsumerModule(t, consumerDir, sourceURL)
	runIACTool(t, tool, consumerDir, "init", "-backend=false", "-input=false", "-no-color")
	runIACTool(t, tool, consumerDir, "validate", "-no-color")
}

// consumeArchiveAtRootWithAllTools extracts the artifact and runs
// `tofu init -backend=false` + `tofu validate` AT the unpacked archive root
// for every available IAC tool. This is the "produce → distribute → consume"
// flow the v1 contract requires for //subdir bundles whose archive root IS
// the entry module (review item 2): no consumer wrapper, no `cd`, no edits.
func consumeArchiveAtRootWithAllTools(t *testing.T, artifactPath string) {
	t.Helper()
	for _, tool := range requireAllIACTools(t) {
		consumeArchiveAtRootWithTool(t, tool, artifactPath)
	}
}

// consumeArchiveAtRootWithTool extracts artifactPath into a tempdir and runs
// `tofu init -backend=false -input=false` + `tofu validate` at the unpacked
// archive root. The bundle's root must BE the entry module for this to pass.
func consumeArchiveAtRootWithTool(t *testing.T, tool iacTool, artifactPath string) {
	t.Helper()
	extractDir := t.TempDir()
	switch {
	case strings.HasSuffix(artifactPath, ".zip"):
		extractZip(t, artifactPath, extractDir)
	case strings.HasSuffix(artifactPath, ".tar.gz"):
		extractTarGz(t, artifactPath, extractDir)
	case strings.HasSuffix(artifactPath, ".tar.xz"):
		extractTarXZ(t, artifactPath, extractDir)
	default:
		t.Fatalf("cannot extract unknown artifact format: %s", artifactPath)
	}
	runIACTool(t, tool, extractDir, "init", "-backend=false", "-input=false", "-no-color")
	runIACTool(t, tool, extractDir, "validate", "-no-color")
}

func expectArchivePlanFailsWithAllTools(t *testing.T, sourceURL string) {
	t.Helper()
	for _, tool := range requireAllIACTools(t) {
		expectArchivePlanFailsWithTool(t, tool, sourceURL)
	}
}

func expectArchivePlanFailsWithTool(t *testing.T, tool iacTool, sourceURL string) {
	t.Helper()
	consumerDir := t.TempDir()
	writeConsumerModule(t, consumerDir, sourceURL)
	runIACTool(t, tool, consumerDir, "init", "-backend=false", "-input=false", "-no-color")
	output := runIACToolError(t, tool, consumerDir, "plan", "-refresh=false", "-input=false", "-no-color")
	assert.Contains(t, output, "Invalid function argument")
	assert.Contains(t, output, "no file exists")
}
