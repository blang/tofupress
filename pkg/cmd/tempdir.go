package cmd

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
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
	"github.com/ulikunitz/xz"

	"github.com/blang/tofupress/pkg/tofupress"
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
	// Add OCI getter for oci:// scheme support
	getters["oci"] = &tofupress.OCIGetter{}

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

	// Detect and extract archives: go-getter may copy a zip/tar.gz/tar.xz file
	// to packageDir without extracting it for local file:// sources.
	// Check if packageDir is a regular file or contains only an archive.
	if err := extractArchiveIfNeeded(packageDir); err != nil {
		cleanup()
		return "", "", nil, fmt.Errorf("failed to extract archive: %w", err)
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
// It preserves file permissions and resolves symlinks to avoid infinite loops.
//
//nolint:gocognit // symlink resolution adds necessary complexity
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

		// Handle symlinks: resolve the target and copy the real content.
		// Symlinks in Terraform module directories are commonly used to share
		// modules across projects (monorepos, multi-environment setups).
		if d.Type()&fs.ModeSymlink != 0 {
			target, readErr := os.Readlink(path)
			if readErr != nil {
				return fmt.Errorf("failed to read symlink %s: %w", relPath, readErr)
			}
			// Resolve the target relative to the symlink's directory
			resolvedPath := filepath.Join(filepath.Dir(path), target)
			info, statErr := os.Stat(resolvedPath)
			if statErr != nil {
				// Self-referencing or dangling symlinks cannot be resolved.
				// Skip them instead of failing — the symlink target may not
				// be relevant to Terraform module resolution.
				return nil //nolint:nilerr // intentionally skipping unresolvable symlinks
			}
			if info.IsDir() {
				// Symlink to a directory: create the directory and copy contents
				if mkdirErr := os.MkdirAll(dstPath, info.Mode()); mkdirErr != nil {
					return mkdirErr
				}
				return copyDir(resolvedPath, dstPath)
			}
			// Symlink to a regular file: copy the target file
			return copyFile(resolvedPath, dstPath, info.Mode())
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

// extractArchiveIfNeeded detects if packageDir is a regular file (not a directory)
// or contains only an archive file, and extracts it in place.
// This handles the case where go-getter copies a local archive file (e.g., .zip, .tar.gz)
// without decompressing it.
//
//nolint:gocognit // straightforward branching for archive detection
func extractArchiveIfNeeded(packageDir string) error {
	// Check if packageDir is a regular file (go-getter may copy it directly)
	info, statErr := os.Stat(packageDir)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return nil // path doesn't exist yet, nothing to do
		}
		return statErr
	}

	if info.IsDir() {
		// It's a directory — check if it contains a single archive file
		entries, readErr := os.ReadDir(packageDir)
		if readErr != nil {
			return nil //nolint:nilerr // best-effort; if we can't read the dir, assume it's not an archive
		}
		if len(entries) != 1 || entries[0].IsDir() {
			return nil
		}
		archiveFile := filepath.Join(packageDir, entries[0].Name())
		archiveName := entries[0].Name()
		lower := strings.ToLower(archiveName)
		if !strings.HasSuffix(lower, ".zip") && !strings.HasSuffix(lower, ".tar.gz") && !strings.HasSuffix(lower, ".tgz") && !strings.HasSuffix(lower, ".tar.xz") && !strings.HasSuffix(lower, ".txz") {
			return nil
		}
		return extractArchiveToDir(archiveFile, packageDir)
	}

	// It's a regular file — extract it
	archiveFile := packageDir
	parentDir := filepath.Dir(packageDir)
	extractedDir := filepath.Join(parentDir, "extracted")

	if err := extractArchiveToDir(archiveFile, extractedDir); err != nil {
		return err
	}

	// Remove the original file
	os.Remove(archiveFile) //nolint:errcheck,gosec // best-effort cleanup

	// Rename extracted directory to packageDir
	if err := os.Rename(extractedDir, packageDir); err != nil {
		return fmt.Errorf("failed to rename extracted directory: %w", err)
	}

	return nil
}

// extractArchiveToDir extracts an archive file to the destination directory.
// Supports .zip, .tar.gz/.tgz, and .tar.xz/.txz formats.
func extractArchiveToDir(archiveFile, destDir string) error {
	lower := strings.ToLower(archiveFile)

	switch {
	case strings.HasSuffix(lower, ".zip"):
		return extractZip(archiveFile, destDir)
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return extractTarGz(archiveFile, destDir)
	case strings.HasSuffix(lower, ".tar.xz"), strings.HasSuffix(lower, ".txz"):
		return extractTarXz(archiveFile, destDir)
	default:
		return fmt.Errorf("unsupported archive format: %s", archiveFile)
	}
}

func extractZip(src, dst string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return fmt.Errorf("failed to open zip: %w", err)
	}
	defer r.Close() //nolint:errcheck // best effort

	for _, f := range r.File {
		//nolint:gosec // G305: zipslip prevention checked below
		targetPath := filepath.Join(dst, f.Name)

		// Prevent zip slip
		if !strings.HasPrefix(filepath.Clean(targetPath), filepath.Clean(dst)+string(os.PathSeparator)) {
			return fmt.Errorf("illegal file path in zip: %s", f.Name)
		}

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(targetPath, 0o755); err != nil { //nolint:gosec // G301: standard permissions
				return err
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil { //nolint:gosec // G301: standard permissions
			return err
		}

		out, err := os.OpenFile(targetPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode()) //nolint:gosec // G304: sanitized above
		if err != nil {
			return err
		}
		rc, openErr := f.Open()
		if openErr != nil {
			//nolint:errcheck,gosec // best effort on error path
			out.Close()
			return openErr
		}
		//nolint:gosec // decompression bomb: archive sources are trusted (user-supplied modules)
		_, copyErr := io.Copy(out, rc)
		//nolint:errcheck,gosec // read-only close
		rc.Close()
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func extractTarGz(src, dst string) error {
	f, err := os.Open(src) //nolint:gosec // G304: path from our own code
	if err != nil {
		return fmt.Errorf("failed to open tar.gz: %w", err)
	}
	defer f.Close() //nolint:errcheck // best effort

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gz.Close() //nolint:errcheck // best effort

	return extractTar(gz, dst)
}

func extractTarXz(src, dst string) error {
	f, err := os.Open(src) //nolint:gosec // G304: path from our own code
	if err != nil {
		return fmt.Errorf("failed to open tar.xz: %w", err)
	}
	defer f.Close() //nolint:errcheck // best effort

	xzReader, err := xz.NewReader(f)
	if err != nil {
		return fmt.Errorf("failed to create xz reader: %w", err)
	}

	return extractTar(xzReader, dst)
}

//nolint:gocognit,gosec // tar extraction with safety checks is inherently complex; zip slip checked above
func extractTar(r io.Reader, dst string) error {
	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read tar entry: %w", err)
		}

		//nolint:gosec // G305: path traversal prevention checked below
		targetPath := filepath.Join(dst, header.Name)

		// Prevent path traversal
		if !strings.HasPrefix(filepath.Clean(targetPath), filepath.Clean(dst)+string(os.PathSeparator)) {
			return fmt.Errorf("illegal file path in tar: %s", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(targetPath, os.FileMode(header.Mode)); err != nil { //nolint:gosec // G115: mode from trusted archive
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil { //nolint:gosec // G301: standard permissions
				return err
			}
			out, createErr := os.OpenFile(targetPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(header.Mode)) //nolint:gosec // G304: sanitized above
			if createErr != nil {
				return createErr
			}
			//nolint:gosec // decompression bomb: archive sources are trusted
			if _, cpErr := io.Copy(out, tr); cpErr != nil {
				//nolint:errcheck,gosec // best effort on error path
				out.Close()
				return cpErr
			}
			if err := out.Close(); err != nil {
				return err
			}
		}
	}
	return nil
}
