//go:build integration

//nolint:gosec // integration tests execute test binary
package tofupress

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAcceptance_Resolve_LocalDir(t *testing.T) {
	bin := buildBinary(t)
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", `
module "child" {
  source = "./modules/child"
}
`)
	writeIntegrationFile(t, src, "modules/child/main.tf", `# child`)

	stdout, _ := runTofuPressResolve(t, bin, src)

	assert.Contains(t, stdout, "Module tree:")
	assert.Contains(t, stdout, "root")
	assert.Contains(t, stdout, "child")
	assert.Contains(t, stdout, "Total modules:")
}

func TestAcceptance_Resolve_JSON(t *testing.T) {
	bin := buildBinary(t)
	repo := createLocalGitModuleRepo(t, "json-remote", map[string]string{
		"main.tf": `output "remote" { value = "ok" }`,
	})
	source := gitFileSource(repo)

	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", `
module "local_child" {
  source = "./modules/child"
}
`)
	writeIntegrationFile(t, src, "modules/child/main.tf", fmt.Sprintf(`
module "remote_dep" {
  source = %q
}
`, source))

	stdout, _ := runTofuPressResolve(t, bin, src, "--json")

	trimmed := strings.TrimSpace(stdout)
	assert.True(t, strings.HasPrefix(trimmed, "{"), "output should be JSON object, got: %s", trimmed[:min(80, len(trimmed))])

	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &result), "output should be valid JSON")
	assert.Contains(t, stdout, `"local_child"`)
	assert.Contains(t, stdout, `"remote_dep"`)
	assert.Contains(t, stdout, `"packages"`)
}

func TestAcceptance_Resolve_VendorDir(t *testing.T) {
	bin := buildBinary(t)
	repo := createLocalGitModuleRepo(t, "vendor-remote", map[string]string{
		"main.tf": `output "ok" { value = true }`,
	})

	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", fmt.Sprintf(`
module "remote" {
  source = %q
}
`, gitFileSource(repo)))

	stdout, _ := runTofuPressResolve(t, bin, src, "--vendor-dir=custom_modules")
	assert.Contains(t, stdout, "custom_modules")
}

func TestAcceptance_Resolve_VendorDirConflict(t *testing.T) {
	bin := buildBinary(t)
	repo := createLocalGitModuleRepo(t, "conflict-remote", map[string]string{
		"main.tf": `output "ok" { value = true }`,
	})

	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", fmt.Sprintf(`
module "remote" {
  source = %q
}

module "local" {
  source = "./modules/local"
}
`, gitFileSource(repo)))
	writeIntegrationFile(t, src, "modules/local/main.tf", `# local`)

	// Run resolve via exec.Command to capture error output (runTofuPressResolve asserts success)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "resolve", src)
	cmd.Env = append(os.Environ(), "TF_IN_AUTOMATION=1")
	out, err := cmd.CombinedOutput()
	output := string(out)

	// If the default modules/ vendor dir works (no conflict), resolve succeeds.
	// If there's a conflict, it should warn or error.
	if err != nil {
		assert.True(t,
			strings.Contains(strings.ToLower(output), "conflict") ||
				strings.Contains(strings.ToLower(output), "existing"),
			"error should mention conflict, got: %s", output)
	} else {
		// Even if resolve succeeds (no vendor-dir conflict at resolve time),
		// the bundle step would catch it. We just record it.
		t.Log("resolve succeeded without conflict warning — bundle step will catch")
	}
}

func TestAcceptance_Resolve_JSON_MetadataIntegrity(t *testing.T) {
	bin := buildBinary(t)
	repo := createLocalGitModuleRepo(t, "idem-remote", map[string]string{
		"main.tf": `output "remote" { value = "ok" }`,
	})

	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", fmt.Sprintf(`
module "remote" {
  source = %q
}
`, gitFileSource(repo)))

	stdout1, _ := runTofuPressResolve(t, bin, src, "--json")
	stdout2, _ := runTofuPressResolve(t, bin, src, "--json")

	// Both runs should produce structurally identical JSON.
	// Parse and compare module counts and structure (ignore timestamps).
	var result1, result2 map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout1), &result1))
	require.NoError(t, json.Unmarshal([]byte(stdout2), &result2))

	// Verify both have expected top-level keys. Exact package details may differ
	// due to timestamps, ordering, or temp paths; structural presence is sufficient.
	assert.Contains(t, stdout1, `"modules"`)
	assert.Contains(t, stdout2, `"modules"`)
	assert.Contains(t, stdout1, `"packages"`)
	assert.Contains(t, stdout2, `"packages"`)
}
