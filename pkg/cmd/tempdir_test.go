//nolint:gosec // tests use explicit fixture permissions and test-owned paths
package cmd

import (
	"archive/zip"
	"context"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/go-getter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/blang/tofupress/pkg/tofupress"
)

func TestArchiveExtractionPath(t *testing.T) {
	root := t.TempDir()
	target, isRoot, err := archiveExtractionPath(root, "modules/vpc/main.tf")
	require.NoError(t, err)
	assert.False(t, isRoot)
	assert.Equal(t, filepath.Join(root, "modules", "vpc", "main.tf"), target)

	target, isRoot, err = archiveExtractionPath(root, "./")
	require.NoError(t, err)
	assert.True(t, isRoot)
	assert.Equal(t, filepath.Clean(root), target)

	for _, unsafe := range []string{"", "../escape", "/absolute", `..\escape`, `C:\escape`, "line\nbreak"} {
		t.Run(unsafe, func(t *testing.T) {
			_, _, err := archiveExtractionPath(root, unsafe)
			require.Error(t, err)
		})
	}
}

func TestExtractArchiveIfNeededReplacesArchiveInsteadOfBundlingIt(t *testing.T) {
	packageDir := t.TempDir()
	archivePath := filepath.Join(packageDir, "module.zip")
	archiveFile, err := os.Create(archivePath)
	require.NoError(t, err)
	zipWriter := zip.NewWriter(archiveFile)
	entry, err := zipWriter.Create("main.tf")
	require.NoError(t, err)
	_, err = entry.Write([]byte(`output "x" { value = true }`))
	require.NoError(t, err)
	require.NoError(t, zipWriter.Close())
	require.NoError(t, archiveFile.Close())

	require.NoError(t, extractArchiveIfNeeded(packageDir))

	assert.FileExists(t, filepath.Join(packageDir, "main.tf"))
	assert.NoFileExists(t, filepath.Join(packageDir, "module.zip"))
}

func TestLocalSourceDoesNotExpandToContainingGitRepository(t *testing.T) {
	repo := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(repo, ".git"), 0o755))
	subject := filepath.Join(repo, "modules")
	require.NoError(t, os.Mkdir(subject, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(subject, "main.tf"), []byte(`output "x" { value = true }`), 0o644))
	if err := os.Symlink("missing-target", filepath.Join(repo, "unrelated-broken-link")); err != nil {
		t.Skip("symlinks not supported on this system")
	}

	resolvers := map[string]func(context.Context, string) (string, string, func(), error){
		"module": resolveSource,
		"tree":   resolveTreeSource,
	}
	for name, resolve := range resolvers {
		t.Run(name, func(t *testing.T) {
			workDir, packageRoot, cleanup, err := resolve(context.Background(), subject)
			require.NoError(t, err)
			defer cleanup()

			assert.Equal(t, packageRoot, workDir)
			assert.FileExists(t, filepath.Join(workDir, "main.tf"))
			assert.NoFileExists(t, filepath.Join(packageRoot, "unrelated-broken-link"))
		})
	}
}

func TestResolveSourceRejectsSubdirectoryTraversal(t *testing.T) {
	packageDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(packageDir, "main.tf"), []byte(`output "x" { value = true }`), 0o644))

	_, _, cleanup, err := resolveSource(context.Background(), packageDir+"//../..")

	if cleanup != nil {
		cleanup()
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), "subdirectory escapes package boundary")
}

func TestResolveSourceWithFetcherRedactsNestedGetterErrors(t *testing.T) {
	const source = "https://alice:" + "hunter2@example.invalid/module?token=topsecret"
	fetcher := tofupress.NewFetcher(tofupress.WithGetters(map[string]getter.Getter{
		"https": leakingCommandGetter{},
	}))

	_, _, cleanup, err := resolveSourceWithFetcher(context.Background(), source, fetcher)

	if cleanup != nil {
		cleanup()
	}
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "alice")
	assert.NotContains(t, err.Error(), "hunter2")
	assert.NotContains(t, err.Error(), "topsecret")
	assert.Contains(t, err.Error(), "token=REDACTED")
}

type leakingCommandGetter struct{}

func (leakingCommandGetter) Get(_ string, source *url.URL) error {
	return fmt.Errorf("custom getter leaked %s", source.String())
}

func (leakingCommandGetter) GetFile(_ string, source *url.URL) error {
	return fmt.Errorf("custom getter leaked %s", source.String())
}

func (leakingCommandGetter) ClientMode(*url.URL) (getter.ClientMode, error) {
	return getter.ClientModeDir, nil
}

func (leakingCommandGetter) SetClient(*getter.Client) {}

func TestCopyDirPreservesPermissions(t *testing.T) {
	// Create a source directory with files of different permissions
	srcDir := t.TempDir()

	// Create a file with 0755 permissions (executable)
	execFile := filepath.Join(srcDir, "script.sh")
	err := os.WriteFile(execFile, []byte("#!/bin/bash\necho hello"), 0o755) //nolint:gosec // G306: test needs executable permissions
	if err != nil {
		t.Fatal(err)
	}

	// Create a file with 0644 permissions (regular file)
	regFile := filepath.Join(srcDir, "data.txt")
	err = os.WriteFile(regFile, []byte("some data"), 0o644) //nolint:gosec // G306: test needs standard file permissions
	if err != nil {
		t.Fatal(err)
	}

	// Create a subdirectory with specific permissions
	subDir := filepath.Join(srcDir, "subdir")
	err = os.Mkdir(subDir, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	// Create destination directory
	dstDir := filepath.Join(t.TempDir(), "dst")

	// Copy the directory
	err = copyDir(srcDir, dstDir)
	if err != nil {
		t.Fatal(err)
	}

	// Verify executable file permissions
	execInfo, err := os.Stat(filepath.Join(dstDir, "script.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if execInfo.Mode().Perm() != 0o755 {
		t.Errorf("executable file permissions: got %o, want %o", execInfo.Mode().Perm(), 0o755)
	}

	// Verify regular file permissions
	regInfo, err := os.Stat(filepath.Join(dstDir, "data.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if regInfo.Mode().Perm() != 0o644 {
		t.Errorf("regular file permissions: got %o, want %o", regInfo.Mode().Perm(), 0o644)
	}

	// Verify subdirectory permissions
	subInfo, err := os.Stat(filepath.Join(dstDir, "subdir"))
	if err != nil {
		t.Fatal(err)
	}
	if subInfo.Mode().Perm() != 0o700 {
		t.Errorf("subdirectory permissions: got %o, want %o", subInfo.Mode().Perm(), 0o700)
	}
}

func TestCopyDirRejectsSymlinkOutsideSource(t *testing.T) {
	srcDir := t.TempDir()
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	require.NoError(t, os.WriteFile(outsideFile, []byte("do-not-copy"), 0o600))
	if err := os.Symlink(outsideFile, filepath.Join(srcDir, "leak.txt")); err != nil {
		t.Skip("symlinks not supported on this system")
	}

	dstDir := filepath.Join(t.TempDir(), "dst")
	err := copyDir(srcDir, dstDir)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes source root")
	assert.NoFileExists(t, filepath.Join(dstDir, "leak.txt"))
}

func TestCopyDirRejectsSymlinkDirectoryCycle(t *testing.T) {
	srcDir := t.TempDir()
	subDir := filepath.Join(srcDir, "sub")
	require.NoError(t, os.Mkdir(subDir, 0o755))
	if err := os.Symlink("..", filepath.Join(subDir, "back-to-root")); err != nil {
		t.Skip("symlinks not supported on this system")
	}

	err := copyDir(srcDir, filepath.Join(t.TempDir(), "dst"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "symlink cycle")
}

//nolint:gocyclo // test covers file symlinks, directory symlinks, and regular files
func TestCopyDirResolvesSymlinks(t *testing.T) {
	// Symlinks should be resolved and their real content copied.
	srcDir := t.TempDir()

	// Create a regular file
	targetFile := filepath.Join(srcDir, "target.txt")
	err := os.WriteFile(targetFile, []byte("target content"), 0o644) //nolint:gosec // G306: test needs standard file permissions
	if err != nil {
		t.Fatal(err)
	}

	// Create a symlink to the file
	linkFile := filepath.Join(srcDir, "link.txt")
	err = os.Symlink("target.txt", linkFile)
	if err != nil {
		t.Skip("symlinks not supported on this system")
	}

	// Create a subdirectory and symlink to it
	subDir := filepath.Join(srcDir, "subdir")
	err = os.Mkdir(subDir, 0o755) //nolint:gosec // G301: standard permissions
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(subDir, "nested.txt"), []byte("nested"), 0o644) //nolint:gosec // G306: standard permissions
	if err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(srcDir, "linked-dir")
	err = os.Symlink("subdir", linkDir)
	if err != nil {
		t.Fatal(err)
	}

	// Copy the directory
	dstDir := filepath.Join(t.TempDir(), "dst")
	err = copyDir(srcDir, dstDir)
	if err != nil {
		t.Fatal(err)
	}

	// Verify the target file was copied
	data, err := os.ReadFile(filepath.Join(dstDir, "target.txt")) //nolint:gosec // G304: test path
	if err != nil {
		t.Fatalf("target file should be copied: %v", err)
	}
	if string(data) != "target content" {
		t.Errorf("target file content mismatch: got %q", data)
	}

	// Verify the symlinked file was resolved and its content copied
	// (should be a regular file, not a symlink)
	info, err := os.Lstat(filepath.Join(dstDir, "link.txt"))
	if err != nil {
		t.Fatalf("symlinked file should be resolved and copied: %v", err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		t.Error("link.txt should be a regular file with resolved content, not a symlink")
	}
	linkData, err := os.ReadFile(filepath.Join(dstDir, "link.txt")) //nolint:gosec // G304: test path
	if err != nil {
		t.Fatal(err)
	}
	if string(linkData) != "target content" {
		t.Errorf("resolved symlink content mismatch: got %q", linkData)
	}

	// Verify the symlinked directory was resolved and its contents copied
	nestedInfo, err := os.Stat(filepath.Join(dstDir, "linked-dir"))
	if err != nil {
		t.Fatalf("symlinked directory should be resolved and copied: %v", err)
	}
	if !nestedInfo.IsDir() {
		t.Error("linked-dir should be a directory")
	}
	nestedData, err := os.ReadFile(filepath.Join(dstDir, "linked-dir", "nested.txt")) //nolint:gosec // G304: test path
	if err != nil {
		t.Fatal(err)
	}
	if string(nestedData) != "nested" {
		t.Errorf("nested file content mismatch: got %q", nestedData)
	}
}
