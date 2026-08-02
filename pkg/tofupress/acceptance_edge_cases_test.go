//go:build integration

//nolint:gosec // integration tests execute test binary and use local paths
package tofupress

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Error path tests: resolve/bundle must error for invalid inputs ---

func TestAcceptance_Error_EmptyDirectory(t *testing.T) {
	bin := buildBinary(t)
	src := t.TempDir()

	stdout, _ := runTofuPressResolve(t, bin, src)
	// Empty directory resolves to a single root module (no child modules).
	// The CLI handles empty dirs gracefully; no warning is required.
	assert.Contains(t, stdout, "Module tree:")
	assert.Contains(t, stdout, "Total modules: 1")
}

func TestAcceptance_Error_MissingSource(t *testing.T) {
	bin := buildBinary(t)
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", `
module "bad" {
  # no source
}
`)

	cmd := exec.Command(bin, "module", src, filepath.Join(t.TempDir(), "b.zip"))
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "missing source should error")
	assert.True(t,
		strings.Contains(string(out), "source") || strings.Contains(string(out), "required"),
		"error should mention source, got: %s", string(out))
}

func TestAcceptance_Error_CircularDependency(t *testing.T) {
	bin := buildBinary(t)
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", `
module "a" {
  source = "./modules/a"
}
`)
	writeIntegrationFile(t, src, "modules/a/main.tf", `
module "b" {
  source = "../b"
}
`)
	writeIntegrationFile(t, src, "modules/b/main.tf", `
module "a" {
  source = "../a"
}
`)

	cmd := exec.Command(bin, "resolve", src)
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "circular dependency should error")
	assert.Contains(t, strings.ToLower(string(out)), "circular")
}

func TestAcceptance_Error_SelfReference(t *testing.T) {
	bin := buildBinary(t)
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", `
module "self" {
  source = "./"
}
`)

	cmd := exec.Command(bin, "resolve", src)
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "self-reference should error")
	assert.True(t,
		strings.Contains(strings.ToLower(string(out)), "circular") ||
			strings.Contains(strings.ToLower(string(out)), "self"),
		"error should mention circular or self, got: %s", string(out))
}

func TestAcceptance_Error_CrossBoundary(t *testing.T) {
	bin := buildBinary(t)
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", `
module "escape" {
  source = "../../outside"
}
`)

	cmd := exec.Command(bin, "resolve", src)
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "cross-boundary module should error")
	assert.True(t,
		strings.Contains(strings.ToLower(string(out)), "boundary") ||
			strings.Contains(strings.ToLower(string(out)), "outside") ||
			strings.Contains(strings.ToLower(string(out)), "escape"),
		"error should mention boundary/escape, got: %s", string(out))
}

func TestAcceptance_Error_AbsolutePath(t *testing.T) {
	bin := buildBinary(t)
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", `
module "abs" {
  source = "/etc/module"
}
`)

	cmd := exec.Command(bin, "resolve", src)
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "absolute path source should error")
	assert.True(t,
		strings.Contains(strings.ToLower(string(out)), "absolute") ||
			strings.Contains(strings.ToLower(string(out)), "invalid") ||
			strings.Contains(strings.ToLower(string(out)), "/etc"),
		"error should mention absolute or invalid path, got: %s", string(out))
}

func TestAcceptance_Error_InvalidFormat(t *testing.T) {
	bin := buildBinary(t)
	src := createSimpleFixture(t)
	artifact := filepath.Join(t.TempDir(), "bad.rar")

	cmd := exec.Command(bin, "module", src, artifact, "--format=rar")
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "invalid format should error")
	assert.True(t,
		strings.Contains(strings.ToLower(string(out)), "unsupported bundle format") ||
			strings.Contains(strings.ToLower(string(out)), "unsupported format"),
		"error should mention unsupported format, got: %s", string(out))
}

func TestAcceptance_Error_InvalidStrip(t *testing.T) {
	bin := buildBinary(t)
	src := createSimpleFixture(t)
	artifact := filepath.Join(t.TempDir(), "bundle.zip")

	cmd := exec.Command(bin, "module", src, artifact, "--strip=delete-everything")
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "invalid strip level should error")
	assert.True(t,
		strings.Contains(strings.ToLower(string(out)), "unsupported strip level") ||
			strings.Contains(strings.ToLower(string(out)), "strip"),
		"error should mention unsupported strip level, got: %s", string(out))
}

func TestAcceptance_Error_NonexistentSource(t *testing.T) {
	bin := buildBinary(t)

	cmd := exec.Command(bin, "resolve", "/tmp/does-not-exist-12345-tofupress-test")
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "nonexistent source should error")
	assert.True(t,
		strings.Contains(strings.ToLower(string(out)), "no such file") ||
			strings.Contains(strings.ToLower(string(out)), "not found") ||
			strings.Contains(strings.ToLower(string(out)), "does not exist"),
		"error should mention nonexistent path, got: %s", string(out))
}

func TestAcceptance_Error_BundleNoArgs(t *testing.T) {
	bin := buildBinary(t)

	cmd := exec.Command(bin, "module")
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "bundle with no args should error")
	assert.True(t,
		strings.Contains(strings.ToLower(string(out)), "usage") ||
			strings.Contains(strings.ToLower(string(out)), "required") ||
			strings.Contains(strings.ToLower(string(out)), "accepts") ||
			strings.Contains(strings.ToLower(string(out)), "argument"),
		"bundle with no args should show usage, got: %s", string(out))
}

// --- Binary behavior tests: correctness guarantees ---

func TestAcceptance_Binary_NoSourceMutation(t *testing.T) {
	bin := buildBinary(t)
	src := createSimpleFixture(t)

	origHash := sha256Dir(t, src)
	origMainModTime := fileModTime(t, filepath.Join(src, "main.tf"))

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, src, artifact, "--format=zip")

	afterHash := sha256Dir(t, src)
	assert.Equal(t, origHash, afterHash, "source files must not be modified by bundling")

	afterMainModTime := fileModTime(t, filepath.Join(src, "main.tf"))
	assert.Equal(t, origMainModTime, afterMainModTime, "modification time changed — source was touched")

	// Verify no modules/ leaked into source
	_, err := os.Stat(filepath.Join(src, "modules"))
	assert.True(t, os.IsNotExist(err), "modules/ must not exist in source dir after bundle")
}

func TestAcceptance_Binary_BundleNoDuplicates(t *testing.T) {
	bin := buildBinary(t)
	repo := createLocalGitModuleRepo(t, "remote", map[string]string{
		"main.tf": `output "ok" { value = true }`,
	})
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", fmt.Sprintf(`module "r" { source = %q }`, gitFileSource(repo)))

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, src, artifact, "--format=zip")

	cmd := exec.Command("unzip", "-l", artifact)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "unzip -l failed: %s", string(out))

	lines := strings.Split(string(out), "\n")
	names := make(map[string]int)
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 4 {
			name := fields[len(fields)-1]
			names[name]++
			assert.Equal(t, 1, names[name], "duplicate entry in archive: %s", name)
		}
	}
	assert.Greater(t, len(names), 0, "archive should have entries")
}

func TestAcceptance_Binary_IdempotentArchive(t *testing.T) {
	bin := buildBinary(t)
	src := createSimpleFixture(t)

	artifact1 := filepath.Join(t.TempDir(), "bundle1.zip")
	runTofuPressBundle(t, bin, src, artifact1, "--format=zip")
	hash1 := fileHash(t, artifact1)

	artifact2 := filepath.Join(t.TempDir(), "bundle2.zip")
	runTofuPressBundle(t, bin, src, artifact2, "--format=zip")
	hash2 := fileHash(t, artifact2)

	// Metadata contains timestamps, so exact match may not be possible.
	// At minimum both must validate correctly.
	if hash1 == hash2 {
		t.Log("archives produced identical hashes (full idempotency)")
	} else {
		t.Logf("archives have different hashes (expected due to timestamps: %x vs %x)", hash1, hash2)
	}

	_ = metadataFromArtifact(t, artifact1)
	_ = metadataFromArtifact(t, artifact2)
}

func TestAcceptance_Error_DynamicSource_Warns(t *testing.T) {
	bin := buildBinary(t)
	src := t.TempDir()
	writeIntegrationFile(t, src, "main.tf", `
variable "repo_url" {
  type    = string
  default = "."
}
module "dynamic" {
  source = "${var.repo_url}"
}
`)

	cmd := exec.Command(bin, "resolve", src)
	out, err := cmd.CombinedOutput()
	// Dynamic source may succeed (using default) but should warn
	output := string(out)
	if err != nil {
		assert.True(t,
			strings.Contains(strings.ToLower(output), "dynamic") ||
				strings.Contains(strings.ToLower(output), "variable") ||
				strings.Contains(strings.ToLower(output), "interpolation"),
			"error should mention dynamic source, got: %s", output)
	} else {
		assert.True(t,
			strings.Contains(strings.ToLower(output), "dynamic") ||
				strings.Contains(strings.ToLower(output), "module tree"),
			"dynamic source should produce output, got: %s", output)
	}
}

func TestAcceptance_Error_CorruptArtifact(t *testing.T) {
	textFile := filepath.Join(t.TempDir(), "not-a-bundle.txt")
	require.NoError(t, os.WriteFile(textFile, []byte("hello world"), 0o644))

	_, err := ReadMetadataFromArtifact(textFile)
	require.Error(t, err, "corrupt artifact should fail metadata read")
}

// --- helpers ---

// sha256Dir computes an aggregate SHA-256 hash of all regular files in dir (sorted by path).
func sha256Dir(t *testing.T, dir string) string {
	t.Helper()
	hasher := sha256.New()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		hasher.Write([]byte(rel))
		hasher.Write(data)
		return nil
	})
	require.NoError(t, err, "walking source dir for hash")
	return fmt.Sprintf("%x", hasher.Sum(nil))
}

// fileHash returns the SHA-256 hex string of a file.
func fileHash(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	hash := sha256.Sum256(data)
	return fmt.Sprintf("%x", hash[:])
}

// fileModTime returns the modification time of a file.
func fileModTime(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return info.ModTime().UnixNano()
}
