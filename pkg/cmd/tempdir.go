package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/blang/tofupress/pkg/tofupress"
)

// resolveSource prepares a working directory from either a local path or remote source.
// For remote sources, it downloads them to a temp directory using go-getter.
// For local sources, it copies them to a temp directory.
// The provided context is used for remote fetch operations.
// Returns the temp directory path and a cleanup function.
func resolveSource(ctx context.Context, source string) (workDir string, cleanup func(), err error) {
	// Classify the source to determine if it's remote
	src := tofupress.ClassifySource(source, "")

	if src.Type == tofupress.SourceLocal {
		// Local source - validate it exists and copy to temp
		if _, statErr := os.Stat(source); statErr != nil {
			if os.IsNotExist(statErr) {
				return "", nil, fmt.Errorf("directory does not exist: %s", source)
			}
			return "", nil, fmt.Errorf("cannot access %s: %w", source, statErr)
		}
		return prepareWorkDir(source)
	}

	// Remote source - download to temp
	tempDir, err := os.MkdirTemp("", "tofupress-remote-*")
	if err != nil {
		return "", nil, fmt.Errorf("failed to create temp directory: %w", err)
	}

	fetcher := tofupress.NewFetcher()
	if err := fetcher.Fetch(ctx, tempDir, source); err != nil {
		os.RemoveAll(tempDir) //nolint:errcheck,gosec // cleanup after error
		return "", nil, fmt.Errorf("failed to fetch remote source: %w", err)
	}

	cleanup = func() {
		os.RemoveAll(tempDir) //nolint:errcheck,gosec // cleanup failures are acceptable
	}

	return tempDir, cleanup, nil
}

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

// copyFile copies a single file from src to dst, preserving content but not permissions.
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
