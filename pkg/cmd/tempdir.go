package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// prepareWorkDir copies the source directory to a temp location for processing.
// Returns the temp directory path and a cleanup function.
func prepareWorkDir(sourceDir string) (workDir string, cleanup func(), err error) {
	// Create temp directory
	tempDir, err := os.MkdirTemp("", "tofupress-*")
	if err != nil {
		return "", nil, fmt.Errorf("failed to create temp directory: %w", err)
	}

	// Copy source to temp
	if err := copyDir(sourceDir, tempDir); err != nil {
		os.RemoveAll(tempDir) //nolint:errcheck,gosec // cleanup after error
		return "", nil, fmt.Errorf("failed to copy source to temp: %w", err)
	}

	cleanup = func() {
		os.RemoveAll(tempDir) //nolint:errcheck,gosec // cleanup failures are acceptable
	}

	return tempDir, cleanup, nil
}

// copyDir recursively copies a directory from src to dst.
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Get relative path
		relPath, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		dstPath := filepath.Join(dst, relPath)

		if info.IsDir() {
			return os.MkdirAll(dstPath, info.Mode())
		}

		// Copy file
		return copyFile(path, dstPath)
	})
}

func copyFile(src, dst string) error {
	srcFile, err := os.Open(src) //nolint:gosec // G304: path is from our own directory walk
	if err != nil {
		return err
	}
	defer srcFile.Close() //nolint:errcheck // best effort

	dstFile, err := os.Create(dst) //nolint:gosec // G304: path is constructed by us
	if err != nil {
		return err
	}
	defer dstFile.Close() //nolint:errcheck // best effort

	_, err = io.Copy(dstFile, srcFile)
	return err
}
