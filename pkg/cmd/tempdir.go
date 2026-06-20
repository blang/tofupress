package cmd

import (
	"context"
	"fmt"
	"io"
	"io/fs"
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

	// Use PackageAddr for fetching (base URL without subpath)
	fetchSource := src.PackageAddr
	if fetchSource == "" {
		fetchSource = source
	}

	fetcher := tofupress.NewFetcher()
	if err := fetcher.Fetch(ctx, tempDir, fetchSource); err != nil {
		os.RemoveAll(tempDir) //nolint:errcheck,gosec // cleanup after error
		return "", nil, fmt.Errorf("failed to fetch remote source: %w", err)
	}

	// Navigate to SubDir if specified
	workDir = tempDir
	if src.SubDir != "" {
		workDir = filepath.Join(tempDir, src.SubDir)
		if _, err := os.Stat(workDir); err != nil {
			os.RemoveAll(tempDir) //nolint:errcheck,gosec // cleanup after error
			return "", nil, fmt.Errorf("subpath %s does not exist in fetched repository: %w", src.SubDir, err)
		}
	}

	cleanup = func() {
		os.RemoveAll(tempDir) //nolint:errcheck,gosec // cleanup failures are acceptable
	}

	return workDir, cleanup, nil
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
// It preserves file permissions and skips symlinks to avoid infinite loops.
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Get relative path
		relPath, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		dstPath := filepath.Join(dst, relPath)

		// Skip symlinks to avoid infinite loops and broken references
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}

		// Get full file info for directories and regular files
		info, err := d.Info()
		if err != nil {
			return err
		}

		if info.IsDir() {
			return os.MkdirAll(dstPath, info.Mode())
		}

		// Copy file with permissions
		return copyFile(path, dstPath, info.Mode())
	})
}

// copyFile copies a single file from src to dst, preserving permissions.
func copyFile(src, dst string, mode fs.FileMode) error {
	srcFile, err := os.Open(src) //nolint:gosec // G304: path is from our own directory walk
	if err != nil {
		return err
	}
	defer srcFile.Close() //nolint:errcheck // read-only close errors are acceptable

	dstFile, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode) //nolint:gosec // G304: path is constructed by us
	if err != nil {
		return err
	}

	_, err = io.Copy(dstFile, srcFile)
	// Check close error for destination file - write operations can fail on close
	// (e.g., filesystem full, network error). Only report close error if no prior error.
	if closeErr := dstFile.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	return err
}
