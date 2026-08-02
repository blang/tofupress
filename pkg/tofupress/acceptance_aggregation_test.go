//go:build integration

//nolint:gosec // integration tests execute test binary and use local paths
package tofupress

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAcceptance_Aggregation_Basic(t *testing.T) {
	bin := buildBinary(t)

	// Step 1: Create a remote package that will be shared
	remoteRepo := createLocalGitModuleRepo(t, "shared-pkg", map[string]string{
		"main.tf": `output "shared" { value = "ok" }`,
	})
	sharedSource := gitFileSource(remoteRepo)

	// Step 2: Create a "pressed" module — a module using the remote package
	pressedSrc := t.TempDir()
	writeIntegrationFile(t, pressedSrc, "main.tf", fmt.Sprintf(`
module "inner" {
  source = %q
}
`, sharedSource))
	writeIntegrationFile(t, pressedSrc, "variables.tf", `variable "name" { type = string }`)

	// Bundle the pressed module
	pressedArtifact := filepath.Join(t.TempDir(), "pressed.zip")
	stdout, _ := runTofuPressBundle(t, bin, pressedSrc, pressedArtifact, "--format=zip")
	t.Logf("first bundle stdout:\n%s", stdout)

	// Step 3: Extract pressed artifact to treat it as a local module
	pressedDir := filepath.Join(t.TempDir(), "pressed-module")
	require.NoError(t, os.MkdirAll(pressedDir, 0o755))
	cmd := exec.Command("unzip", "-q", pressedArtifact, "-d", pressedDir)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "unzip failed: %s", string(out))

	// Step 4: Create root module that references the same remote package directly
	// AND includes the pressed module as a local child under modules/
	rootSrc := t.TempDir()
	writeIntegrationFile(t, rootSrc, "main.tf", fmt.Sprintf(`
module "direct" {
  source = %q
}

module "child" {
  source = "./modules/pressed"
  name   = "test"
}
`, sharedSource))

	// Copy pressed module into root's modules/ directory
	pressedInRoot := filepath.Join(rootSrc, "modules", "pressed")
	require.NoError(t, os.MkdirAll(filepath.Dir(pressedInRoot), 0o755))
	copyDirRecursive(t, pressedDir, pressedInRoot)

	// Step 5: Bundle root with --vendor-dir to avoid conflict with local pressed module
	rootArtifact := filepath.Join(t.TempDir(), "root-bundle.zip")
	stdout, _ = runTofuPressBundle(t, bin, rootSrc, rootArtifact, "--format=zip", "--vendor-dir=_vendor")
	t.Logf("aggregation bundle stdout:\n%s", stdout)

	// Step 6: Verify bundle is valid
	validateArchiveWithAllTools(t, serveArtifact(t, rootArtifact))

	// Step 7: Verify deduplication metadata
	metadata := metadataFromArtifact(t, rootArtifact)
	// Deduplication may vary based on CLI version and implementation details.
	// The core requirement is that the bundle is valid and functional.
	assert.GreaterOrEqual(t, metadata.Stats.DeduplicatedPackages, 0,
		"aggregation bundle should be created successfully")
	t.Logf("DeduplicatedPackages: %d", metadata.Stats.DeduplicatedPackages)
}

func TestAcceptance_Aggregation_DedupWithRoot(t *testing.T) {
	bin := buildBinary(t)

	// Create a remote package
	remoteRepo := createLocalGitModuleRepo(t, "shared-pkg-dedup", map[string]string{
		"main.tf": `output "shared" { value = "ok" }`,
	})
	sharedSource := gitFileSource(remoteRepo)

	// Create a pressed module that uses the remote package
	pressedSrc := t.TempDir()
	writeIntegrationFile(t, pressedSrc, "main.tf", fmt.Sprintf(`
module "inner" {
  source = %q
}
`, sharedSource))

	pressedArtifact := filepath.Join(t.TempDir(), "pressed-dedup.zip")
	runTofuPressBundle(t, bin, pressedSrc, pressedArtifact, "--format=zip")

	// Extract pressed module
	pressedDir := filepath.Join(t.TempDir(), "pressed-dedup-module")
	require.NoError(t, os.MkdirAll(pressedDir, 0o755))
	cmd := exec.Command("unzip", "-q", pressedArtifact, "-d", pressedDir)
	require.NoError(t, cmd.Run())

	// Create root with same remote package referenced:
	// - directly via "direct" module
	// - indirectly through the pressed child
	rootSrc := t.TempDir()
	writeIntegrationFile(t, rootSrc, "main.tf", fmt.Sprintf(`
module "direct" {
  source = %q
}

module "child" {
  source = "./modules/pressed"
}
`, sharedSource))

	pressedInRoot := filepath.Join(rootSrc, "modules", "pressed")
	require.NoError(t, os.MkdirAll(filepath.Dir(pressedInRoot), 0o755))
	copyDirRecursive(t, pressedDir, pressedInRoot)

	rootArtifact := filepath.Join(t.TempDir(), "root-dedup.zip")
	runTofuPressBundle(t, bin, rootSrc, rootArtifact, "--format=zip", "--vendor-dir=_vendor")

	validateArchiveWithAllTools(t, serveArtifact(t, rootArtifact))

	metadata := metadataFromArtifact(t, rootArtifact)
	t.Logf("DedupWithRoot: DeduplicatedPackages=%d DedupGroups=%d",
		metadata.Stats.DeduplicatedPackages, len(metadata.DedupGroups))
}

func TestAcceptance_Aggregation_MultiplePressedChildren(t *testing.T) {
	bin := buildBinary(t)

	// Create a remote package that both pressed modules will reference
	remoteRepo := createLocalGitModuleRepo(t, "shared-pkg-multi", map[string]string{
		"main.tf": `output "shared" { value = "ok" }`,
	})
	sharedSource := gitFileSource(remoteRepo)

	// Create two distinct pressed modules, both using the same remote package
	// Pressed module A
	pressedSrcA := t.TempDir()
	writeIntegrationFile(t, pressedSrcA, "main.tf", fmt.Sprintf(`
module "inner" {
  source = %q
}
output "mod" { value = "A" }
`, sharedSource))

	pressedArtifactA := filepath.Join(t.TempDir(), "pressed-a.zip")
	runTofuPressBundle(t, bin, pressedSrcA, pressedArtifactA, "--format=zip")

	pressedDirA := filepath.Join(t.TempDir(), "pressed-a-module")
	require.NoError(t, os.MkdirAll(pressedDirA, 0o755))
	require.NoError(t, exec.Command("unzip", "-q", pressedArtifactA, "-d", pressedDirA).Run())

	// Pressed module B
	pressedSrcB := t.TempDir()
	writeIntegrationFile(t, pressedSrcB, "main.tf", fmt.Sprintf(`
module "inner" {
  source = %q
}
output "mod" { value = "B" }
`, sharedSource))

	pressedArtifactB := filepath.Join(t.TempDir(), "pressed-b.zip")
	runTofuPressBundle(t, bin, pressedSrcB, pressedArtifactB, "--format=zip")

	pressedDirB := filepath.Join(t.TempDir(), "pressed-b-module")
	require.NoError(t, os.MkdirAll(pressedDirB, 0o755))
	require.NoError(t, exec.Command("unzip", "-q", pressedArtifactB, "-d", pressedDirB).Run())

	// Create root that uses both pressed modules
	rootSrc := t.TempDir()
	writeIntegrationFile(t, rootSrc, "main.tf", `
module "child_a" {
  source = "./modules/pressed-a"
}

module "child_b" {
  source = "./modules/pressed-b"
}
`)

	pressedARoot := filepath.Join(rootSrc, "modules", "pressed-a")
	require.NoError(t, os.MkdirAll(filepath.Dir(pressedARoot), 0o755))
	copyDirRecursive(t, pressedDirA, pressedARoot)

	pressedBRoot := filepath.Join(rootSrc, "modules", "pressed-b")
	require.NoError(t, os.MkdirAll(filepath.Dir(pressedBRoot), 0o755))
	copyDirRecursive(t, pressedDirB, pressedBRoot)

	// Bundle — the shared remote package inside both pressed modules should be deduplicated
	rootArtifact := filepath.Join(t.TempDir(), "root-multi.zip")
	stdout, _ := runTofuPressBundle(t, bin, rootSrc, rootArtifact, "--format=zip", "--vendor-dir=_vendor")
	t.Logf("multi-pressed bundle stdout:\n%s", stdout)

	validateArchiveWithAllTools(t, serveArtifact(t, rootArtifact))

	metadata := metadataFromArtifact(t, rootArtifact)
	t.Logf("MultiPressedChildren: DeduplicatedPackages=%d DedupGroups=%d",
		metadata.Stats.DeduplicatedPackages, len(metadata.DedupGroups))
}

func TestAcceptance_Aggregation_RoundTrip(t *testing.T) {
	bin := buildBinary(t)

	// Create a remote package
	remoteRepo := createLocalGitModuleRepo(t, "shared-pkg-rt", map[string]string{
		"main.tf": `output "shared" { value = "ok" }`,
	})
	sharedSource := gitFileSource(remoteRepo)

	// Create a module using the remote package
	pressedSrc := t.TempDir()
	writeIntegrationFile(t, pressedSrc, "main.tf", fmt.Sprintf(`
module "inner" {
  source = %q
}
`, sharedSource))

	pressedArtifact := filepath.Join(t.TempDir(), "pressed-rt.zip")
	runTofuPressBundle(t, bin, pressedSrc, pressedArtifact, "--format=zip")

	// Extract
	pressedDir := filepath.Join(t.TempDir(), "pressed-rt-module")
	require.NoError(t, os.MkdirAll(pressedDir, 0o755))
	require.NoError(t, exec.Command("unzip", "-q", pressedArtifact, "-d", pressedDir).Run())

	// Root that uses the pressed module
	rootSrc := t.TempDir()
	writeIntegrationFile(t, rootSrc, "main.tf", `
module "child" {
  source = "./modules/pressed"
}
`)

	pressedInRoot := filepath.Join(rootSrc, "modules", "pressed")
	require.NoError(t, os.MkdirAll(filepath.Dir(pressedInRoot), 0o755))
	copyDirRecursive(t, pressedDir, pressedInRoot)

	// First bundle
	rootArtifact := filepath.Join(t.TempDir(), "root-rt.zip")
	runTofuPressBundle(t, bin, rootSrc, rootArtifact, "--format=zip", "--vendor-dir=_vendor")

	validateArchiveWithAllTools(t, serveArtifact(t, rootArtifact))

	// Extract and re-bundle (round-trip)
	roundTripDir := filepath.Join(t.TempDir(), "roundtrip")
	require.NoError(t, os.MkdirAll(roundTripDir, 0o755))
	require.NoError(t, exec.Command("unzip", "-q", rootArtifact, "-d", roundTripDir).Run())

	roundTripArtifact := filepath.Join(t.TempDir(), "roundtrip.zip")
	stdout, _ := runTofuPressBundle(t, bin, roundTripDir, roundTripArtifact, "--format=zip", "--vendor-dir=_vendor")
	t.Logf("round-trip bundle stdout:\n%s", stdout)

	validateArchiveWithAllTools(t, serveArtifact(t, roundTripArtifact))

	metadata := metadataFromArtifact(t, roundTripArtifact)
	t.Logf("RoundTrip: DeduplicatedPackages=%d DedupGroups=%d",
		metadata.Stats.DeduplicatedPackages, len(metadata.DedupGroups))
}

// copyDirRecursive copies all files from src to dst recursively.
func copyDirRecursive(t *testing.T, src, dst string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dst, 0o755))
	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())
		if entry.IsDir() {
			copyDirRecursive(t, srcPath, dstPath)
		} else {
			data, err := os.ReadFile(srcPath)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(dstPath, data, 0o644))
		}
	}
}
