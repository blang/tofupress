//go:build integration

//nolint:gosec // integration tests execute test binaries and access local paths
package tofupress

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// testBinaryPath holds the path to the pre-compiled tofupress binary.
// Built once in TestMain for the entire package.
var testBinaryPath string

// TestMain builds the tofupress binary once before all integration tests.
func TestMain(m *testing.M) {
	testDir, err := os.MkdirTemp("", "tofupress-acceptance-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create test temp dir: %v\n", err) //nolint:errcheck // stderr is best-effort
		os.Exit(1)
	}
	defer os.RemoveAll(testDir) //nolint:errcheck,gosec // cleanup failures are acceptable

	binaryName := "tofupress"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	testBinaryPath = filepath.Join(testDir, binaryName)

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		fmt.Fprintln(os.Stderr, "failed to determine test file location")
		os.Exit(1)
	}
	projectRoot := filepath.Dir(filepath.Dir(filepath.Dir(filename)))

	cmd := exec.Command("go", "build", "-o", testBinaryPath, "./cmd/tofupress") //nolint:gosec // G204: subprocess is intentional for building test binary
	cmd.Dir = projectRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to build test binary: %v\n%s\n", err, output) //nolint:errcheck // stderr is best-effort
		os.Exit(1)
	}

	// Enable provider cache if directory exists
	if cacheDir := os.Getenv("TF_PLUGIN_CACHE_DIR"); cacheDir != "" {
		if _, err := os.Stat(cacheDir); err == nil {
			os.Setenv("TF_PLUGIN_CACHE_DIR", cacheDir)
		}
	}

	os.Exit(m.Run())
}

// buildBinary returns the path to the pre-compiled tofupress binary.
func buildBinary(t *testing.T) string {
	t.Helper()
	require.NotEmpty(t, testBinaryPath, "test binary path should be set by TestMain")
	_, err := os.Stat(testBinaryPath)
	require.NoError(t, err, "test binary should exist at %s", testBinaryPath)
	return testBinaryPath
}

// BranchSpec defines a git branch to create with its files.
type BranchSpec struct {
	Name  string
	Files map[string]string // relative path -> content
}

// TagSpec defines a git tag pointing to a branch.
type TagSpec struct {
	Name   string
	Branch string
}

// createLocalGitRepo creates a local git repo with multiple branches and tags.
// Returns the repo directory path (a t.TempDir subdirectory).
func createLocalGitRepo(t *testing.T, name string, branches []BranchSpec, tags []TagSpec) string {
	t.Helper()
	repoDir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(repoDir, 0o755)) //nolint:gosec // G301: test needs standard permissions

	runCommand(t, repoDir, nil, "git", "init")
	runCommand(t, repoDir, nil, "git", "config", "user.email", "integration@example.invalid")
	runCommand(t, repoDir, nil, "git", "config", "user.name", "TofuPress Acceptance")

	for _, branch := range branches {
		if branch.Name != "main" && branch.Name != "master" {
			branchExists := false
			for _, prev := range branches {
				if prev.Name == branch.Name {
					branchExists = true
					break
				}
			}
			if branchExists && branch.Name == branches[0].Name {
				continue // Already handled as first branch
			}
			runCommand(t, repoDir, nil, "git", "checkout", "-b", branch.Name)
			writeBranchFiles(t, repoDir, branch.Files)
			runCommand(t, repoDir, nil, "git", "add", ".")
			runCommand(t, repoDir, nil, "git", "commit", "--allow-empty", "-m", "feat: "+branch.Name)
		} else {
			writeBranchFiles(t, repoDir, branch.Files)
			runCommand(t, repoDir, nil, "git", "add", ".")
			runCommand(t, repoDir, nil, "git", "commit", "--allow-empty", "-m", "initial")
			runCommand(t, repoDir, nil, "git", "branch", "-M", branch.Name)
		}
	}

	// Only tag after all branches are created
	for _, tag := range tags {
		runCommand(t, repoDir, nil, "git", "tag", tag.Name, tag.Branch)
	}

	return repoDir
}

// writeBranchFiles writes all files in the map to the repo directory.
func writeBranchFiles(t *testing.T, repoDir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		writeIntegrationFile(t, repoDir, rel, content)
	}
}

// gitTaggedSource returns a git::file:// source URL with the given tag reference.
func gitTaggedSource(repoDir, tag string) string {
	return "git::" + (&url.URL{Scheme: "file", Path: repoDir}).String() + "?ref=" + tag
}

// runTofuPressResolve runs `tofupress resolve` with flags and returns stdout and stderr.
func runTofuPressResolve(t *testing.T, bin, source string, flags ...string) (stdout string, stderr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	args := append([]string{"resolve", source}, flags...)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = filepath.Dir(bin)
	cmd.Env = append(os.Environ(), "TF_IN_AUTOMATION=1")
	var outBuilder, errBuilder strings.Builder
	cmd.Stdout = &outBuilder
	cmd.Stderr = &errBuilder
	err := cmd.Run()
	require.NoError(t, err, "tofupress %s failed\nstdout:\n%s\nstderr:\n%s",
		strings.Join(args, " "), outBuilder.String(), errBuilder.String())
	return outBuilder.String(), errBuilder.String()
}

// requireOciCredentials skips the test if DOCKER_USERNAME or DOCKER_PASSWORD are not set.
func requireOciCredentials(t *testing.T) {
	t.Helper()
	if os.Getenv("DOCKER_USERNAME") == "" || os.Getenv("DOCKER_PASSWORD") == "" {
		t.Skip("skipping OCI test: DOCKER_USERNAME and DOCKER_PASSWORD not set")
	}
}

// ociEnv returns environment variables for oras authentication.
func ociEnv() []string {
	return []string{
		"ORAS_USER=" + os.Getenv("DOCKER_USERNAME"),
		"ORAS_PASS=" + os.Getenv("DOCKER_PASSWORD"),
	}
}

// ociPush pushes an artifact to an OCI registry via oras.
func ociPush(t *testing.T, image, tag, artifact string) {
	t.Helper()
	ref := fmt.Sprintf("%s:%s", image, tag)
	cmd := exec.Command("oras", "push",
		"--artifact-type=application/vnd.opentofu.modulepkg",
		ref, artifact+":application/vnd.opentofu.modulepkg")
	cmd.Env = append(os.Environ(), ociEnv()...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "oras push %s failed: %s", ref, string(out))
}

// ociPull pulls an artifact from an OCI registry via oras.
func ociPull(t *testing.T, image, tag, dir string) {
	t.Helper()
	ref := fmt.Sprintf("%s:%s", image, tag)
	cmd := exec.Command("oras", "pull", "--output", dir, ref)
	cmd.Env = append(os.Environ(), ociEnv()...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "oras pull %s failed: %s", ref, string(out))
}
