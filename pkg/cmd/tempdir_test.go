package cmd

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

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
