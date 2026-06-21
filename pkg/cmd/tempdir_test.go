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

func TestCopyDirHandlesSymlinks(t *testing.T) {
	// Create a source directory with a symlink
	srcDir := t.TempDir()

	// Create a regular file
	targetFile := filepath.Join(srcDir, "target.txt")
	err := os.WriteFile(targetFile, []byte("target content"), 0o644) //nolint:gosec // G306: test needs standard file permissions
	if err != nil {
		t.Fatal(err)
	}

	// Create a symlink
	linkFile := filepath.Join(srcDir, "link.txt")
	err = os.Symlink("target.txt", linkFile)
	if err != nil {
		t.Skip("symlinks not supported on this system")
	}

	// Create destination directory
	dstDir := filepath.Join(t.TempDir(), "dst")

	// Copy the directory
	err = copyDir(srcDir, dstDir)
	if err != nil {
		t.Fatal(err)
	}

	// Verify the target file was copied
	_, err = os.Stat(filepath.Join(dstDir, "target.txt"))
	if err != nil {
		t.Errorf("target file should be copied: %v", err)
	}

	// Verify the symlink is handled - it should be skipped to avoid infinite loops
	// and broken references in the temp directory
	linkInfo, err := os.Lstat(filepath.Join(dstDir, "link.txt"))
	if err == nil {
		// If it exists, verify it's not a regular file (symlinks should be skipped)
		if linkInfo.Mode()&fs.ModeSymlink == 0 {
			t.Errorf("symlink should be skipped, but found regular file at link.txt")
		}
	}
	// If it doesn't exist, that's the expected behavior (skipped)
}
