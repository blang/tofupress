package cmd

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	pathpkg "path"
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
//
// Local sources are never expanded by sniffing for a surrounding git
// repository. A caller that needs parent/sibling content must opt in explicitly
// with package//entry, which gives the copy and resolver a visible boundary.
//
// Returns the work directory, package root (for boundary enforcement), and cleanup function.
func resolveSource(ctx context.Context, source string) (workDir, packageRoot string, cleanup func(), err error) {
	return resolveSourceWithFetcher(ctx, source, tofupress.NewFetcher())
}

func resolveSourceWithFetcher(
	ctx context.Context,
	source string,
	fetcher *tofupress.Fetcher,
) (workDir, packageRoot string, cleanup func(), err error) {
	return resolveSourcePackage(ctx, source, fetcher)
}

// resolveTreeSource is the tree adapter's acquisition seam. An explicit //
// source may name a larger fetch package, but ResolveTree still treats the
// selected subject as its semantic boundary (ADR-0002).
func resolveTreeSource(ctx context.Context, source string) (workDir, packageRoot string, cleanup func(), err error) {
	return resolveTreeSourceWithFetcher(ctx, source, tofupress.NewFetcher())
}

func resolveTreeSourceWithFetcher(
	ctx context.Context,
	source string,
	fetcher *tofupress.Fetcher,
) (workDir, packageRoot string, cleanup func(), err error) {
	return resolveSourcePackage(ctx, source, fetcher)
}

//nolint:gocognit,gocyclo // source acquisition has scheme and archive branches
func resolveSourcePackage(
	ctx context.Context,
	source string,
	fetcher *tofupress.Fetcher,
) (workDir, packageRoot string, cleanup func(), err error) {
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

	if fetcher == nil {
		cleanup()
		return "", "", nil, fmt.Errorf("source fetcher is nil")
	}

	// Use the same owned Fetcher graph as dependency resolution. This keeps
	// strict-OCI policy, warning routing, getter isolation, and error redaction
	// consistent for the root subject and every nested module.
	if err := fetcher.Fetch(ctx, packageDir, detected); err != nil {
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

	// Navigate to a subdirectory only after proving its canonical path remains
	// inside the fetched package. A source suffix such as //../../../etc must
	// never turn the root subject into an arbitrary host directory.
	workDir = packageDir
	if subDir != "" {
		workDir = filepath.Join(packageDir, subDir)
		if boundaryErr := ensureSourceSubdir(packageDir, workDir); boundaryErr != nil {
			cleanup()
			return "", "", nil, fmt.Errorf("invalid package subdirectory %q: %w", subDir, boundaryErr)
		}
	}

	return workDir, packageRoot, cleanup, nil
}

func ensureSourceSubdir(packageRoot, target string) error {
	root, err := canonicalCopyPath(packageRoot)
	if err != nil {
		return fmt.Errorf("resolve package root: %w", err)
	}
	resolved, err := canonicalCopyPath(target)
	if err != nil {
		return fmt.Errorf("subdirectory does not exist: %w", err)
	}
	copier := dereferenceCopier{root: root}
	if boundaryErr := copier.ensureWithinRoot(target, resolved); boundaryErr != nil {
		return fmt.Errorf("subdirectory escapes package boundary: %w", boundaryErr)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("subdirectory is not a directory")
	}
	return nil
}

// splitPackageSubdir detects whether the given address string has a subdirectory
// portion (marked by //), and if so returns a non-empty subDir string along with
// the trimmed package address.
// This is adapted from OpenTofu's implementation.
func splitPackageSubdir(src string) (packageAddr, subDir string) {
	return tofupress.SplitPackageSubdir(src)
}

// copyDir recursively copies a directory from src to dst. Symlinks are
// dereferenced for archive portability, but every resolved target must remain
// within src so a module cannot import arbitrary host files into an artifact.
func copyDir(src, dst string) error {
	root, err := filepath.EvalSymlinks(src)
	if err != nil {
		return fmt.Errorf("resolve source root %s: %w", src, err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("make source root absolute: %w", err)
	}
	root = filepath.Clean(root)
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("inspect source root %s: %w", src, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("source root %s is not a directory", src)
	}

	copier := dereferenceCopier{root: root}
	return copier.copyDir(root, dst, make(map[string]bool))
}

type dereferenceCopier struct {
	root string
}

func (c dereferenceCopier) copyDir(src, dst string, active map[string]bool) error {
	canonical, err := canonicalCopyPath(src)
	if err != nil {
		return fmt.Errorf("resolve directory %s: %w", src, err)
	}
	if boundaryErr := c.ensureWithinRoot(src, canonical); boundaryErr != nil {
		return boundaryErr
	}
	if active[canonical] {
		return fmt.Errorf("symlink cycle while copying %s: directory %s is already active", src, canonical)
	}
	active[canonical] = true
	defer delete(active, canonical)

	info, err := os.Stat(canonical)
	if err != nil {
		return err
	}
	if mkdirErr := os.MkdirAll(dst, info.Mode().Perm()); mkdirErr != nil {
		return mkdirErr
	}
	entries, err := os.ReadDir(canonical)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		sourcePath := filepath.Join(canonical, entry.Name())
		destPath := filepath.Join(dst, entry.Name())
		entryInfo, statErr := os.Lstat(sourcePath)
		if statErr != nil {
			return statErr
		}
		if copyErr := c.copyEntry(sourcePath, destPath, entryInfo, active); copyErr != nil {
			return copyErr
		}
	}
	return nil
}

func canonicalCopyPath(sourcePath string) (string, error) {
	resolved, err := filepath.EvalSymlinks(sourcePath)
	if err != nil {
		return "", err
	}
	absolute, err := filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	return filepath.Clean(absolute), nil
}

func (c dereferenceCopier) copyEntry(sourcePath, destPath string, info os.FileInfo, active map[string]bool) error {
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return c.copySymlink(sourcePath, destPath, active)
	case info.IsDir():
		return c.copyDir(sourcePath, destPath, active)
	case info.Mode().IsRegular():
		return copyFile(sourcePath, destPath, info.Mode())
	default:
		return fmt.Errorf("unsupported source entry type %s at %s", info.Mode().Type(), sourcePath)
	}
}

func (c dereferenceCopier) copySymlink(sourcePath, destPath string, active map[string]bool) error {
	resolved, err := canonicalCopyPath(sourcePath)
	if err != nil {
		return fmt.Errorf("failed to resolve symlink %s (dangling link or symlink cycle): %w", sourcePath, err)
	}
	if boundaryErr := c.ensureWithinRoot(sourcePath, resolved); boundaryErr != nil {
		return boundaryErr
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return err
	}
	switch {
	case info.IsDir():
		return c.copyDir(resolved, destPath, active)
	case info.Mode().IsRegular():
		return copyFile(resolved, destPath, info.Mode())
	default:
		return fmt.Errorf("unsupported symlink target type %s at %s", info.Mode().Type(), sourcePath)
	}
}

func (c dereferenceCopier) ensureWithinRoot(sourcePath, resolved string) error {
	rel, err := filepath.Rel(c.root, resolved)
	if err != nil {
		return fmt.Errorf("compare symlink target %s with source root: %w", resolved, err)
	}
	if rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("symlink %s resolves to %s, which escapes source root %s", sourcePath, resolved, c.root)
	}
	return nil
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
		return extractArchiveReplacingPath(archiveFile, packageDir)
	}

	// It's a regular file — extract it and replace that path with a directory.
	return extractArchiveReplacingPath(packageDir, packageDir)
}

func extractArchiveReplacingPath(archiveFile, packagePath string) (err error) {
	stagingDir, err := os.MkdirTemp(filepath.Dir(packagePath), ".tofupress-extract-*")
	if err != nil {
		return fmt.Errorf("failed to create archive extraction directory: %w", err)
	}
	defer func() {
		if removeErr := os.RemoveAll(stagingDir); removeErr != nil {
			err = errors.Join(err, removeErr)
		}
	}()
	if err := extractArchiveToDir(archiveFile, stagingDir); err != nil {
		return err
	}
	if err := os.RemoveAll(packagePath); err != nil {
		return fmt.Errorf("failed to replace downloaded archive: %w", err)
	}
	if err := os.Rename(stagingDir, packagePath); err != nil {
		return fmt.Errorf("failed to publish extracted archive: %w", err)
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

	for _, file := range r.File {
		if err := extractZipEntry(file, dst); err != nil {
			return err
		}
	}
	return nil
}

func extractZipEntry(file *zip.File, dst string) error {
	targetPath, isRoot, err := archiveExtractionPath(dst, file.Name)
	if err != nil {
		return fmt.Errorf("illegal file path in zip: %s", file.Name)
	}
	if isRoot && file.FileInfo().IsDir() {
		return nil
	}
	if isRoot {
		return fmt.Errorf("illegal root file entry in zip: %s", file.Name)
	}
	if file.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("unsupported symlink entry in zip: %s", file.Name)
	}
	if file.FileInfo().IsDir() {
		return os.MkdirAll(targetPath, 0o755) //nolint:gosec // G301: standard permissions
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil { //nolint:gosec // G301: standard permissions
		return err
	}
	return extractZipFile(file, targetPath)
}

func extractZipFile(file *zip.File, targetPath string) error {
	out, err := os.OpenFile(targetPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, file.Mode()) //nolint:gosec // G304: validated extraction path
	if err != nil {
		return err
	}
	rc, err := file.Open()
	if err != nil {
		_ = out.Close()
		return err
	}
	//nolint:gosec // decompression limits remain a follow-up for source acquisition
	_, copyErr := io.Copy(out, rc)
	_ = rc.Close()
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
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

func archiveExtractionPath(dst, name string) (target string, isRoot bool, err error) {
	if name == "" || strings.ContainsAny(name, "\\:\x00") {
		return "", false, fmt.Errorf("unsafe archive path")
	}
	for _, r := range name {
		if r < ' ' || r == 0x7f {
			return "", false, fmt.Errorf("unsafe archive path")
		}
	}
	cleaned := pathpkg.Clean(name)
	if pathpkg.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", false, fmt.Errorf("archive path escapes destination")
	}
	if cleaned == "." {
		return filepath.Clean(dst), true, nil
	}
	return filepath.Join(dst, filepath.FromSlash(cleaned)), false, nil
}

//nolint:gocognit,gosec // tar extraction with safety checks is inherently complex
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

		targetPath, isRoot, pathErr := archiveExtractionPath(dst, header.Name)
		if pathErr != nil {
			return fmt.Errorf("illegal file path in tar: %s", header.Name)
		}
		if isRoot && header.Typeflag == tar.TypeDir {
			continue
		}
		if isRoot {
			return fmt.Errorf("illegal root file entry in tar: %s", header.Name)
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
			//nolint:gosec // decompression limits remain a follow-up for source acquisition
			if _, cpErr := io.Copy(out, tr); cpErr != nil {
				//nolint:errcheck,gosec // best effort on error path
				out.Close()
				return cpErr
			}
			if err := out.Close(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported tar entry type %d at %s", header.Typeflag, header.Name)
		}
	}
	return nil
}
