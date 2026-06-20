package cmd

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/hashicorp/go-getter"
)

// resolveSource prepares a working directory from a go-getter compatible source.
// It uses go-getter to handle ALL source types uniformly (local, git, http, s3, etc.).
// The // separator defines the package boundary: everything before is the package,
// everything after is the subdirectory within the package.
// Returns the work directory, package root (for boundary enforcement), and cleanup function.
func resolveSource(ctx context.Context, source string) (workDir, packageRoot string, cleanup func(), err error) {
	// Get current working directory for relative path resolution
	pwd, err := os.Getwd()
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to get working directory: %w", err)
	}

	// Split package address from subdirectory using // separator FIRST
	// This must happen before detection, as go-getter doesn't understand //
	packageAddr, subDir := splitPackageSubdir(source)

	// Detect and normalize the package source using go-getter
	// pwd is required for resolving relative paths
	detected, err := getter.Detect(packageAddr, pwd, getter.Detectors)
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to detect source type: %w", err)
	}

	// Create temp directory for the package
	tempDir, err := os.MkdirTemp("", "tofupress-*")
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to create temp directory: %w", err)
	}

	cleanup = func() {
		os.RemoveAll(tempDir) //nolint:errcheck,gosec // cleanup failures are acceptable
	}

	// Create a subdirectory for the actual download
	// go-getter expects the destination to not exist
	packageDir := filepath.Join(tempDir, "package")

	// Configure getters to copy files instead of creating symlinks for local directories
	getters := make(map[string]getter.Getter)
	maps.Copy(getters, getter.Getters)
	// Override FileGetter to copy instead of symlink
	getters["file"] = &getter.FileGetter{
		Copy: true,
	}

	// Download/copy the package using go-getter
	client := &getter.Client{
		Ctx:       ctx,
		Src:       detected,
		Dst:       packageDir,
		Pwd:       pwd,
		Mode:      getter.ClientModeDir,
		Detectors: getter.Detectors,
		Getters:   getters,
	}

	if err := client.Get(); err != nil {
		cleanup()
		return "", "", nil, fmt.Errorf("failed to fetch source: %w", err)
	}

	// CRITICAL: go-getter's FileGetter may create a symlink even with Copy: true
	// We need to resolve it to an actual directory copy
	if info, err := os.Lstat(packageDir); err == nil && info.Mode()&os.ModeSymlink != 0 {
		// It's a symlink - resolve it and copy the actual contents
		realPath, err := filepath.EvalSymlinks(packageDir)
		if err != nil {
			cleanup()
			return "", "", nil, fmt.Errorf("failed to resolve symlink: %w", err)
		}

		// Remove the symlink
		if err := os.Remove(packageDir); err != nil {
			cleanup()
			return "", "", nil, fmt.Errorf("failed to remove symlink: %w", err)
		}

		// Create a real directory and copy contents
		if err := os.MkdirAll(packageDir, 0o755); err != nil { //nolint:gosec // G301: package dir needs standard permissions
			cleanup()
			return "", "", nil, fmt.Errorf("failed to create package directory: %w", err)
		}

		// Copy all contents from the real path to packageDir
		if err := copyDir(realPath, packageDir); err != nil {
			cleanup()
			return "", "", nil, fmt.Errorf("failed to copy source contents: %w", err)
		}
	}

	// Remove .git directory if present (clean up VCS metadata)
	gitDir := filepath.Join(packageDir, ".git")
	if _, err := os.Stat(gitDir); err == nil {
		os.RemoveAll(gitDir) //nolint:errcheck,gosec // best effort cleanup
	}

	// Package root is the package directory (everything downloaded is within this boundary)
	packageRoot = packageDir

	// Navigate to subdirectory if specified
	workDir = packageDir
	if subDir != "" {
		workDir = filepath.Join(packageDir, subDir)
		if _, err := os.Stat(workDir); err != nil {
			cleanup()
			return "", "", nil, fmt.Errorf("subdirectory %s does not exist in package: %w", subDir, err)
		}
	}

	return workDir, packageRoot, cleanup, nil
}

// splitPackageSubdir detects whether the given address string has a subdirectory
// portion (marked by //), and if so returns a non-empty subDir string along with
// the trimmed package address.
// This is adapted from OpenTofu's implementation.
func splitPackageSubdir(src string) (packageAddr, subDir string) {
	// URL might contain another URL in query parameters
	stop := len(src)
	if idx := strings.Index(src, "?"); idx > -1 {
		stop = idx
	}

	// Calculate an offset to avoid accidentally marking the scheme as the dir
	var offset int
	if idx := strings.Index(src[:stop], "://"); idx > -1 {
		offset = idx + 3
	}

	// Check for explicit subdir marker
	idx := strings.Index(src[offset:stop], "//")
	if idx == -1 {
		return src, ""
	}

	idx += offset
	subdir := src[idx+2:]
	src = src[:idx]

	// Reattach query parameters to package address
	if qIdx := strings.Index(subdir, "?"); qIdx > -1 {
		query := subdir[qIdx:]
		subdir = subdir[:qIdx]
		src += query
	}

	return src, path.Clean(subdir)
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
