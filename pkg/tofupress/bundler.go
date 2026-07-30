package tofupress

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"

	"github.com/ulikunitz/xz"
)

// BundleFormat represents the archive format to use for bundling.
type BundleFormat string

// Bundle format constants define the supported archive output formats.
const (
	BundleFormatAuto  BundleFormat = "auto"   // Infer from output file extension
	BundleFormatZIP   BundleFormat = "zip"    // OCI distribution
	BundleFormatTarGZ BundleFormat = "tar.gz" // HTTP distribution
	BundleFormatTarXZ BundleFormat = "tar.xz" // S3 distribution
)

// Directory names to skip during bundling.
const (
	dirNameTerraform = ".terraform"
	dirNameGit       = ".git"
)

// defaultVendorDir is the fallback vendor directory name. It is a private name that
// cannot collide with the conventional Terraform "modules/" directory where users
// keep their own local modules. Running tofupress with the default must never fail due
// to a vendor-dir collision with user content (review finding F1).
const defaultVendorDir = "_vendor"

// vendorDirName returns the vendored modules directory name for a tree.
func vendorDirName(tree *ResolvedTree) string {
	if tree != nil && tree.VendorDir != "" {
		return tree.VendorDir
	}
	return defaultVendorDir
}

// VendorDirName returns the vendored modules directory name that will be used for a tree,
// falling back to the safe default when the tree has no explicit vendor dir.
func VendorDirName(tree *ResolvedTree) string { return vendorDirName(tree) }

// ValidateVendorDir verifies that a vendor directory is one portable path
// component. VendorDir is a name, not an arbitrary output path: allowing
// separators or generated/reserved names could write outside the subject,
// disappear from the archive, or overwrite tofupress metadata.
func ValidateVendorDir(name string) error {
	if name == "" {
		return nil // empty means the private default
	}
	if strings.TrimSpace(name) != name || name == "." || name == ".." ||
		filepath.IsAbs(name) || strings.ContainsAny(name, `/\\:`) {
		return fmt.Errorf("invalid vendor directory %q: use one relative directory name without path separators", name)
	}
	switch name {
	case dirNameGit, dirNameTerraform, MetadataDir:
		return fmt.Errorf("invalid vendor directory %q: that name is reserved by tofupress", name)
	default:
		return nil
	}
}

// ParseBundleFormat parses a format string into a BundleFormat.
func ParseBundleFormat(s string) (BundleFormat, error) {
	switch strings.ToLower(s) {
	case "auto", "":
		return BundleFormatAuto, nil
	case "zip":
		return BundleFormatZIP, nil
	case "tar.gz", "tgz":
		return BundleFormatTarGZ, nil
	case "tar.xz", "txz":
		return BundleFormatTarXZ, nil
	default:
		return "", fmt.Errorf("unsupported bundle format: %s (use zip, tar.gz, or tar.xz)", s)
	}
}

// DetectFormatFromPath infers the bundle format from the output file extension.
// Returns the detected format and true if recognized, or an empty format and false if not.
func DetectFormatFromPath(path string) (BundleFormat, bool) {
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".tar.xz"), strings.HasSuffix(lower, ".txz"):
		return BundleFormatTarXZ, true
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return BundleFormatTarGZ, true
	case strings.HasSuffix(lower, ".zip"):
		return BundleFormatZIP, true
	default:
		return "", false
	}
}

// Bundler creates archives from resolved module trees.
//
//nolint:govet // field alignment is not critical for this type
type Bundler struct {
	Format       BundleFormat
	OCICompliant bool              // When true, creates OCI-compliant bundle without sourcetree/ metadata
	Metadata     *ArtifactMetadata // Optional metadata to embed in the archive
	StripPlan    *StripPlan        // Optional include/exclude plan for safe stripping
	VendorDir    string            // Vendored modules directory name (from tree.VendorDir)
}

// BundlerOption configures a Bundler at construction time (review item 5 /
// ArchAudit R4). Exported fields remain settable post-construction for
// backwards compatibility; options are the preferred, freeze-safe entry point.
type BundlerOption func(*Bundler)

// WithOCICompliant toggles OCI-compliant bundling.
func WithOCICompliant(v bool) BundlerOption { return func(b *Bundler) { b.OCICompliant = v } }

// WithMetadata attaches metadata to embed in the archive.
func WithMetadata(m *ArtifactMetadata) BundlerOption { return func(b *Bundler) { b.Metadata = m } }

// WithStripPlan attaches a pre-computed strip plan.
func WithStripPlan(p *StripPlan) BundlerOption { return func(b *Bundler) { b.StripPlan = p } }

// WithVendorDir sets the vendored modules directory name.
func WithVendorDir(d string) BundlerOption { return func(b *Bundler) { b.VendorDir = d } }

// NewBundler creates a new Bundler with the specified format and options.
func NewBundler(format BundleFormat, opts ...BundlerOption) *Bundler {
	b := &Bundler{Format: format}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Bundle creates a single self-contained archive containing the root module together
// with every reachable local and remote module. Remote packages are vendored under
// <vendorDir>/<pkgID>/ and the resolver has already rewritten the referencing module
// sources to point there; local module references (./sibling, ../ext) are preserved
// verbatim because whole packages are copied intact.
//
// OCI-compliant and non-OCI bundles now share ONE staging pipeline (review F4/F5/F12).
// They differ only in archive format: OCI requires zip. Every format is produced from
// the same staged directory, so the same tree always yields a consistent layout.
func (b *Bundler) Bundle(ctx context.Context, tree *ResolvedTree, outputPath string) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if tree == nil {
		return fmt.Errorf("tree is nil")
	}
	if tree.Root == nil || tree.Root.InstallDir == "" {
		return fmt.Errorf("tree root is missing its install directory")
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}

	format, formatErr := b.resolvedFormat(outputPath)
	if formatErr != nil {
		return formatErr
	}
	if b.OCICompliant && format != BundleFormatZIP {
		return fmt.Errorf("--oci-compliant requires zip format (got %s)", format)
	}

	vendorDir := b.vendorDir(tree)
	if validationErr := b.validateBundleTree(tree, vendorDir); validationErr != nil {
		return validationErr
	}

	// All formats share one staging and commit transaction. Repivot happens
	// inside stageBundle (a no-op outside //subdir pivot mode).
	stagingDir, stageErr := stageBundle(tree, vendorDir, b.StripPlan, b.Metadata)
	if stageErr != nil {
		return stageErr
	}
	defer func() {
		if rmErr := os.RemoveAll(stagingDir); rmErr != nil {
			err = errors.Join(err, rmErr)
		}
	}()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if aggregateErr := b.aggregatePressedModulesInStaging(tree, stagingDir); aggregateErr != nil {
		return fmt.Errorf("failed to aggregate pressed modules: %w", aggregateErr)
	}

	return writeArchiveAtomically(ctx, outputPath, func(tempPath string) error {
		return b.writeStagedArchive(ctx, format, stagingDir, tempPath)
	})
}

func (b *Bundler) vendorDir(tree *ResolvedTree) string {
	if tree != nil && tree.VendorDir != "" {
		return tree.VendorDir
	}
	if b.VendorDir != "" {
		return b.VendorDir
	}
	return defaultVendorDir
}

func (b *Bundler) validateBundleTree(tree *ResolvedTree, vendorDir string) error {
	if err := ValidateVendorDir(vendorDir); err != nil {
		return err
	}
	if err := b.validateVendorDir(tree, vendorDir); err != nil {
		return err
	}
	if err := validatePivotLocalModuleGeometry(tree); err != nil {
		return err
	}
	return b.validatePivotFilesystemRefs(tree)
}

func (b *Bundler) resolvedFormat(outputPath string) (BundleFormat, error) {
	if b.Format != BundleFormatAuto {
		return b.Format, nil
	}
	detected, ok := DetectFormatFromPath(outputPath)
	if !ok {
		return "", fmt.Errorf("could not infer bundle format from output path %q; "+
			"pass --format=zip, --format=tar.gz, or --format=tar.xz", outputPath)
	}
	return detected, nil
}

func (b *Bundler) writeStagedArchive(ctx context.Context, format BundleFormat, stagingDir, outputPath string) error {
	switch format {
	case BundleFormatZIP:
		return b.bundleZipFromDir(ctx, stagingDir, outputPath)
	case BundleFormatTarGZ:
		return b.bundleTarGzFromDir(ctx, stagingDir, outputPath)
	case BundleFormatTarXZ:
		return b.bundleTarXzFromDir(ctx, stagingDir, outputPath)
	default:
		return fmt.Errorf("unsupported format: %s", format)
	}
}

// validatePivotFilesystemRefs rejects entry-module filesystem reads whose
// runtime meaning would change when a //subdir module is moved to archive root.
// A tree press preserves package geometry and is the explicit remedy.
func (b *Bundler) validatePivotFilesystemRefs(tree *ResolvedTree) error {
	if !isPivotedModuleTree(tree) {
		return nil
	}
	refs, err := b.pivotFilesystemRefs(tree)
	if err != nil {
		return err
	}
	for i := range refs {
		ref := &refs[i]
		if ref.ModuleKey != tree.Root.Key {
			continue
		}
		if validationErr := validatePivotFilesystemRef(tree, ref); validationErr != nil {
			return validationErr
		}
	}
	return nil
}

func validatePivotLocalModuleGeometry(tree *ResolvedTree) error {
	if !isPivotedModuleTree(tree) {
		return nil
	}
	packageRoot, err := canonicalStagingPath(tree.Root.PackageRoot)
	if err != nil {
		return fmt.Errorf("resolve pivot package root: %w", err)
	}
	for _, module := range tree.AllModules {
		if module == nil || module == tree.Root || !module.IsLocal || module.InstallDir == "" {
			continue
		}
		installDir, installErr := canonicalStagingPath(module.InstallDir)
		if installErr != nil {
			return fmt.Errorf("resolve local module %q during pivot validation: %w", module.Name, installErr)
		}
		if installDir == packageRoot {
			return fmt.Errorf(
				"module press cannot preserve local module %q because it resolves to the package root while the selected entry is pivoted to archive root; press the package layout with `tofupress tree` instead",
				module.Name,
			)
		}
	}
	return nil
}

func isPivotedModuleTree(tree *ResolvedTree) bool {
	return tree != nil && tree.Root != nil && tree.Root.PackageRoot != "" &&
		filepath.Clean(tree.Root.PackageRoot) != filepath.Clean(tree.Root.InstallDir)
}

func (b *Bundler) pivotFilesystemRefs(tree *ResolvedTree) ([]FilesystemFunctionRef, error) {
	if b.StripPlan != nil {
		return b.StripPlan.FilesystemFunctions, nil
	}
	refs, err := DetectTreeFilesystemFunctions(tree)
	if err != nil {
		return nil, fmt.Errorf("inspect pivoted module filesystem references: %w", err)
	}
	return refs, nil
}

func validatePivotFilesystemRef(tree *ResolvedTree, ref *FilesystemFunctionRef) error {
	if !ref.Static && ref.Kind == refKindFilesystemFunction {
		return pivotFilesystemRefError(tree, ref, "has a dynamic path whose location cannot be proven")
	}
	if ref.Static {
		if err := validateStaticPivotPaths(tree, ref); err != nil {
			return err
		}
	}
	if ref.Kind == refKindPathTemplateRisk && ref.Function == pathModuleFunction && pathModuleTemplateEscapes(ref.RawPath) {
		return pivotFilesystemRefError(tree, ref, "contains parent traversal outside the entry module")
	}
	return nil
}

func validateStaticPivotPaths(tree *ResolvedTree, ref *FilesystemFunctionRef) error {
	paths := append([]string(nil), ref.IncludedPaths...)
	if ref.ResolvedBase != "" {
		paths = append(paths, ref.ResolvedBase)
	}
	for _, referencedPath := range paths {
		if referencedPath == "" {
			continue
		}
		if err := ensureWithinPackage(tree.Root.InstallDir, referencedPath); err != nil {
			reason := fmt.Sprintf("resolves outside the entry module to %s", relPathNoLeak(referencedPath, tree.Root.PackageRoot))
			return pivotFilesystemRefError(tree, ref, reason)
		}
	}
	return nil
}

func pathModuleTemplateEscapes(raw string) bool {
	const marker = "${path.module}/"
	_, suffix, found := strings.Cut(raw, marker)
	if !found {
		return false
	}
	if dynamicAt := strings.Index(suffix, "${"); dynamicAt >= 0 {
		suffix = suffix[:dynamicAt]
	}
	suffix = strings.Trim(suffix, `"' `)
	cleaned := pathpkg.Clean(filepath.ToSlash(suffix))
	return cleaned == ".." || strings.HasPrefix(cleaned, "../")
}

func pivotFilesystemRefError(tree *ResolvedTree, ref *FilesystemFunctionRef, reason string) error {
	return fmt.Errorf(
		"module press cannot preserve %s reference %q in %s when pivoting %s to archive root: %s; press the package layout with `tofupress tree` instead",
		ref.Function,
		ref.RawPath,
		relPathNoLeak(ref.SourceFile, tree.Root.PackageRoot),
		relPathNoLeak(tree.Root.InstallDir, tree.Root.PackageRoot),
		reason,
	)
}

// rootArchiveDir returns the directory that should serve as the archive root.
//
// For //subdir package-boundary inputs, the entry module lives at
// InstallDir while the rest of the package is staged around it. The OCI
// module-package contract requires the zip root to BE the default module, so
// the archive root pivots to the entry subdir (review item 2). For non-pivot
// inputs (PackageRoot unset, or equal to InstallDir), InstallDir IS the
// package root and there is nothing to pivot.
func rootArchiveDir(tree *ResolvedTree) string {
	if tree == nil || tree.Root == nil {
		return ""
	}
	return tree.Root.InstallDir
}

// writeArchiveAtomically writes an archive to a sibling temporary file and
// renames it into place only after the writer has closed, fsynced, and passed a
// final cancellation check. Existing good artifacts therefore survive failed
// or canceled writes.
func writeArchiveAtomically(ctx context.Context, outputPath string, write func(string) error) (err error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}

	outputDir := filepath.Dir(outputPath)
	tempFile, err := os.CreateTemp(outputDir, "."+filepath.Base(outputPath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create archive transaction: %w", err)
	}
	tempPath := tempFile.Name()
	defer func() {
		if removeErr := os.Remove(tempPath); removeErr != nil && !os.IsNotExist(removeErr) {
			err = errors.Join(err, removeErr)
		}
	}()
	if closeErr := tempFile.Close(); closeErr != nil {
		return fmt.Errorf("close archive transaction file: %w", closeErr)
	}

	if writeErr := write(tempPath); writeErr != nil {
		return writeErr
	}
	if chmodErr := os.Chmod(tempPath, 0o644); chmodErr != nil { //nolint:gosec // artifact is intentionally readable
		return fmt.Errorf("set archive permissions: %w", chmodErr)
	}
	if syncErr := syncArchiveFile(tempPath); syncErr != nil {
		return syncErr
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if renameErr := os.Rename(tempPath, outputPath); renameErr != nil {
		return fmt.Errorf("commit archive transaction: %w", renameErr)
	}
	return nil
}

func syncArchiveFile(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec // path is our transaction file
	if err != nil {
		return fmt.Errorf("open archive transaction for sync: %w", err)
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if joinedErr := errors.Join(syncErr, closeErr); joinedErr != nil {
		return fmt.Errorf("sync archive transaction: %w", joinedErr)
	}
	return nil
}

// bundleZipFromDir creates a ZIP archive from a directory.
//
//nolint:gocognit,gocyclo // directory walking and zip creation is inherently complex
func (b *Bundler) bundleZipFromDir(ctx context.Context, srcDir, outputPath string) (err error) {
	outFile, err := os.Create(outputPath) //nolint:gosec // G304: path is our transaction file
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer func() {
		if closeErr := outFile.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()

	zipWriter := zip.NewWriter(outFile)
	defer func() {
		if closeErr := zipWriter.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()

	//nolint:nakedret // named return needed to propagate deferred close errors
	err = filepath.Walk(srcDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}

		// Skip .terraform and .git directories. Content is pre-staged, so no
		// vendor-dir skip is needed or correct here.
		if info.IsDir() && (info.Name() == dirNameTerraform || info.Name() == dirNameGit) {
			return filepath.SkipDir
		}

		if b.StripPlan != nil && !b.StripPlan.IncludePath(path, info.IsDir()) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		relPath, relErr := filepath.Rel(srcDir, path)
		if relErr != nil {
			return relErr
		}

		if relPath == "." {
			return nil
		}

		archivePath, pathErr := portableArchivePath(relPath)
		if pathErr != nil {
			return pathErr
		}
		if info.IsDir() {
			archivePath += "/"
		}

		header, headerErr := zip.FileInfoHeader(info)
		if headerErr != nil {
			return headerErr
		}
		header.Name = archivePath
		header.Method = zip.Deflate

		writer, createErr := zipWriter.CreateHeader(header)
		if createErr != nil {
			return createErr
		}

		if !info.IsDir() {
			switch {
			case info.Mode().IsRegular():
				if copyErr := copyFileWithContext(ctx, writer, path); copyErr != nil {
					return copyErr
				}
			case info.Mode()&os.ModeSymlink != 0:
				target, readErr := os.Readlink(path)
				if readErr != nil {
					return readErr
				}
				if _, writeErr := io.WriteString(writer, target); writeErr != nil {
					return writeErr
				}
			default:
				return fmt.Errorf("unsupported archive entry type %s at %s", info.Mode().Type(), path)
			}
		}

		return nil
	})
	return //nolint:nakedret // named return needed to propagate deferred close errors
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// copyFileWithContext opens and closes one input within the current walk
// iteration. This intentionally avoids deferring file closes in archive loops,
// which otherwise keeps every input descriptor open until the whole walk ends.
func copyFileWithContext(ctx context.Context, dst io.Writer, path string) error {
	file, err := os.Open(path) //nolint:gosec // path comes from our staged tree
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(dst, contextReader{ctx: ctx, reader: file})
	if copyErr == nil {
		copyErr = ctx.Err()
	}
	return errors.Join(copyErr, file.Close())
}

// bundleTarGzFromDir creates a tar.gz archive from a pre-staged directory.
func (b *Bundler) bundleTarGzFromDir(ctx context.Context, srcDir, outputPath string) error {
	return b.bundleTarFromDir(ctx, srcDir, outputPath, func(writer io.Writer) (io.WriteCloser, error) {
		return gzip.NewWriter(writer), nil
	})
}

// bundleTarFromDir contains the one tar walk shared by all compressors.
//
//nolint:gocognit,gocyclo // directory walking and tar creation is inherently complex
func (b *Bundler) bundleTarFromDir(
	ctx context.Context,
	srcDir, outputPath string,
	newCompressor func(io.Writer) (io.WriteCloser, error),
) (err error) {
	outFile, err := os.Create(outputPath) //nolint:gosec // G304: path is our transaction file
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer func() {
		if closeErr := outFile.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()

	compressor, err := newCompressor(outFile)
	if err != nil {
		return fmt.Errorf("create archive compressor: %w", err)
	}
	defer func() {
		if closeErr := compressor.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()

	tarWriter := tar.NewWriter(compressor)
	defer func() {
		if closeErr := tarWriter.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()

	//nolint:nakedret // named return needed to propagate deferred close errors
	err = filepath.Walk(srcDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}

		if info.IsDir() {
			switch info.Name() {
			case dirNameTerraform, dirNameGit:
				return filepath.SkipDir
			}
		}

		// Apply strip plan if present (defensive — files should already be stripped)
		if b.StripPlan != nil && !b.StripPlan.IncludePath(path, info.IsDir()) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		relPath, relErr := filepath.Rel(srcDir, path)
		if relErr != nil {
			return relErr
		}
		if relPath == "." {
			return nil
		}

		linkTarget := ""
		if info.Mode()&os.ModeSymlink != 0 {
			linkTarget, relErr = os.Readlink(path)
			if relErr != nil {
				return relErr
			}
		} else if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported archive entry type %s at %s", info.Mode().Type(), path)
		}
		archivePath, pathErr := portableArchivePath(relPath)
		if pathErr != nil {
			return pathErr
		}
		header, headerErr := tar.FileInfoHeader(info, linkTarget)
		if headerErr != nil {
			return headerErr
		}
		header.Name = archivePath

		if writeErr := tarWriter.WriteHeader(header); writeErr != nil {
			return writeErr
		}

		if info.Mode().IsRegular() {
			if copyErr := copyFileWithContext(ctx, tarWriter, path); copyErr != nil {
				return copyErr
			}
		}

		return nil
	})
	return //nolint:nakedret // named return needed to propagate deferred close errors
}

// bundleTarXzFromDir creates a tar.xz archive from a pre-staged directory.
func (b *Bundler) bundleTarXzFromDir(ctx context.Context, srcDir, outputPath string) error {
	return b.bundleTarFromDir(ctx, srcDir, outputPath, func(writer io.Writer) (io.WriteCloser, error) {
		return xz.NewWriter(writer)
	})
}

// copyDirOCI copies a directory recursively while dereferencing symlinks only
// when their canonical targets remain inside src. Generated .terraform and VCS
// .git directories are skipped; ordinary user directory names are preserved.
func copyDirOCI(src, dst string, stripPlan *StripPlan) error {
	return copyDirOCIWithin(src, dst, src, stripPlan)
}

// copyDirOCIWithin is used when src is an entry subdirectory but the explicit
// package boundary is broader. A link may target that package, but never the
// caller's surrounding filesystem.
func copyDirOCIWithin(src, dst, allowedRoot string, stripPlan *StripPlan) error {
	root, err := canonicalStagingPath(allowedRoot)
	if err != nil {
		return fmt.Errorf("resolve staging copy root %s: %w", allowedRoot, err)
	}
	copier := stagingCopier{allowedRoot: root, stripPlan: stripPlan}
	return copier.copyDir(src, dst, make(map[string]bool))
}

type stagingCopier struct {
	allowedRoot string
	stripPlan   *StripPlan
}

func (c stagingCopier) copyDir(src, dst string, active map[string]bool) error {
	canonical, err := canonicalStagingPath(src)
	if err != nil {
		return fmt.Errorf("resolve staging directory %s: %w", src, err)
	}
	if boundaryErr := c.ensureWithinRoot(src, canonical); boundaryErr != nil {
		return boundaryErr
	}
	if active[canonical] {
		return fmt.Errorf("symlink cycle while staging %s: directory %s is already active", src, canonical)
	}
	active[canonical] = true
	defer delete(active, canonical)

	info, err := os.Stat(canonical)
	if err != nil {
		return err
	}
	if mkdirErr := os.MkdirAll(dst, info.Mode().Perm()); mkdirErr != nil { //nolint:gosec // preserve source permissions
		return mkdirErr
	}
	entries, err := os.ReadDir(canonical)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		sourcePath := filepath.Join(canonical, entry.Name())
		targetPath := filepath.Join(dst, entry.Name())
		entryInfo, err := os.Lstat(sourcePath)
		if err != nil {
			return err
		}
		if shouldSkipStagingEntry(entryInfo, sourcePath, c.stripPlan) {
			continue
		}
		if err := c.copyEntry(sourcePath, targetPath, entryInfo, active); err != nil {
			return err
		}
	}
	return nil
}

func shouldSkipStagingEntry(info os.FileInfo, sourcePath string, stripPlan *StripPlan) bool {
	if info.IsDir() && (info.Name() == dirNameTerraform || info.Name() == dirNameGit) {
		return true
	}
	return stripPlan != nil && !stripPlan.IncludePath(sourcePath, info.IsDir())
}

func (c stagingCopier) copyEntry(sourcePath, targetPath string, info os.FileInfo, active map[string]bool) error {
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return c.copySymlink(sourcePath, targetPath, active)
	case info.IsDir():
		return c.copyDir(sourcePath, targetPath, active)
	case info.Mode().IsRegular():
		return copyFileOCI(sourcePath, targetPath, info.Mode())
	default:
		return fmt.Errorf("unsupported staging entry type %s at %s", info.Mode().Type(), sourcePath)
	}
}

func (c stagingCopier) copySymlink(sourcePath, targetPath string, active map[string]bool) error {
	resolved, err := canonicalStagingPath(sourcePath)
	if err != nil {
		return fmt.Errorf("resolve staging symlink %s (dangling link or symlink cycle): %w", sourcePath, err)
	}
	if boundaryErr := c.ensureWithinRoot(sourcePath, resolved); boundaryErr != nil {
		return boundaryErr
	}
	if isGeneratedOrVCSPath(resolved) {
		return nil
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return err
	}
	switch {
	case info.IsDir():
		return c.copyDir(resolved, targetPath, active)
	case info.Mode().IsRegular():
		return copyFileOCI(resolved, targetPath, info.Mode())
	default:
		return fmt.Errorf("unsupported staging symlink target type %s at %s", info.Mode().Type(), sourcePath)
	}
}

func (c stagingCopier) ensureWithinRoot(sourcePath, resolved string) error {
	rel, err := filepath.Rel(c.allowedRoot, resolved)
	if err != nil {
		return fmt.Errorf("compare staging symlink target %s with package root: %w", resolved, err)
	}
	if relativePathEscapesRoot(rel) {
		return fmt.Errorf("staging symlink %s resolves outside package root", sourcePath)
	}
	return nil
}

func canonicalStagingPath(sourcePath string) (string, error) {
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

// copyFileOCI copies a single file for OCI bundling.
func copyFileOCI(src, dst string, mode os.FileMode) error {
	// Ensure parent directory exists
	if mkErr := os.MkdirAll(filepath.Dir(dst), 0o755); mkErr != nil { //nolint:gosec // G301: standard permissions
		return mkErr
	}

	srcFile, openErr := os.Open(src) //nolint:gosec // G304: path comes from our own tree
	if openErr != nil {
		return openErr
	}
	defer srcFile.Close() //nolint:errcheck // read-only close

	dstFile, createErr := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode) //nolint:gosec // G304: path is constructed by us
	if createErr != nil {
		return createErr
	}
	defer dstFile.Close() //nolint:errcheck // best-effort close

	_, copyErr := io.Copy(dstFile, srcFile)
	return copyErr
}

// stageBundle creates a staging directory and copies the root module,
// local modules outside root, and all packages into it, applying the
// strip plan during copy. The staging dir is populated so that a single
// walk can create the archive without any vendor-dir skip logic.
//
// The caller is responsible for removing the staging directory.
func stageBundle(tree *ResolvedTree, vendorDir string, stripPlan *StripPlan, metadata *ArtifactMetadata) (string, error) {
	if tree == nil || tree.Root == nil {
		return "", fmt.Errorf("cannot stage bundle for nil tree")
	}

	stagingDir, err := os.MkdirTemp("", "tofupress-stage-*")
	if err != nil {
		return "", fmt.Errorf("failed to create staging directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(stagingDir) }

	// 1. Copy root module from archive root to staging root
	archiveRoot := rootArchiveDir(tree)
	rootBoundary := tree.Root.PackageRoot
	if rootBoundary == "" {
		rootBoundary = archiveRoot
	}
	if err := copyDirOCIWithin(archiveRoot, stagingDir, rootBoundary, stripPlan); err != nil {
		cleanup()
		return "", fmt.Errorf("failed to copy root module to staging: %w", err)
	}

	// 2. Stage in-package siblings (//subdir pivot mode) at their package-relative
	// path so the entry module's `../../modules/x` references resolve at the
	// unpacked archive root (review item 2). Source rewrites happen earlier in
	// repivotMonorepoSources; this only stages the bytes.
	if err := stageMonorepoSiblings(tree, stagingDir, stripPlan); err != nil {
		cleanup()
		return "", err
	}
	// Rewrite only the staged entry. The caller's resolved workspace is
	// read-only to Bundler, including in //subdir pivot mode.
	if err := repivotMonorepoSources(tree, stagingDir); err != nil {
		cleanup()
		return "", fmt.Errorf("failed to repivot monorepo sources: %w", err)
	}

	// 3. Copy local modules that live outside the package root (and outside the
	// archive root) into the staging directory at their module-key path.
	if err := stageLocalModules(tree, archiveRoot, stagingDir, stripPlan); err != nil {
		cleanup()
		return "", err
	}

	// 4. Populate vendor directory with packages
	if err := stagePackages(tree, filepath.Join(stagingDir, vendorDir), stripPlan); err != nil {
		cleanup()
		return "", err
	}

	// 5. Write metadata file if provided. Never overwrite subject content at
	// the reserved metadata path: fail explicitly so full-mode pressing cannot
	// silently replace a user's .tofupress/meta.json.
	if metadata != nil {
		metaPath := filepath.Join(stagingDir, MetadataRelPath)
		if _, err := os.Lstat(metaPath); err == nil {
			cleanup()
			return "", fmt.Errorf("archive path %q is reserved for embedded tofupress metadata and conflicts with subject content", MetadataRelPath)
		} else if !os.IsNotExist(err) {
			cleanup()
			return "", fmt.Errorf("failed to inspect reserved metadata path %q: %w", MetadataRelPath, err)
		}
		metaDir := filepath.Dir(metaPath)
		if err := os.MkdirAll(metaDir, 0o755); err != nil { //nolint:gosec // G301: standard dir perms
			cleanup()
			return "", fmt.Errorf("failed to create metadata directory: %w", err)
		}
		if err := WriteMetadataFile(metaPath, metadata); err != nil {
			cleanup()
			return "", fmt.Errorf("failed to write metadata: %w", err)
		}
	}

	return stagingDir, nil
}

// stageLocalModules copies local modules that live outside the archive root into
// the staging directory at their module-key path. Modules inside the archive root
// are already copied by the root module walk; modules inside the package root in
// //subdir pivot mode are staged by stageMonorepoSiblings at their package-relative
// path. Both classes are skipped here.
func stageLocalModules(tree *ResolvedTree, archiveRoot, stagingDir string, stripPlan *StripPlan) error {
	for _, module := range tree.AllModules {
		if module == tree.Root {
			continue
		}
		// Modules whose install dir belongs to a downloaded package are already
		// copied with that whole package. Package maps are keyed by identity, not
		// source address, so address lookup here would miss them and duplicate
		// package internals at attacker-controlled module-key paths.
		if !module.IsLocal || module.InstallDir == "" || moduleInsideDownloadedPackage(tree, module) {
			continue
		}
		// Skip modules inside the archive root — already copied by step 1.
		relPath, err := filepath.Rel(archiveRoot, module.InstallDir)
		if err != nil || !relativePathEscapesRoot(relPath) {
			continue
		}
		// Skip modules inside the package root in pivot mode — staged by
		// stageMonorepoSiblings at their package-relative path.
		if tree.Root != nil && tree.Root.PackageRoot != "" && tree.Root.PackageRoot != archiveRoot {
			if relPkg, relErr := filepath.Rel(tree.Root.PackageRoot, module.InstallDir); relErr == nil && !relativePathEscapesRoot(relPkg) {
				continue
			}
		}
		if err := stageLocalModule(module, stagingDir, stripPlan); err != nil {
			return err
		}
	}
	return nil
}

func stageLocalModule(module *ModuleNode, stagingDir string, stripPlan *StripPlan) error {
	archivePath := strings.ReplaceAll(module.Key, ".", "/")
	targetPath, err := joinPathWithin(stagingDir, archivePath)
	if err != nil {
		return fmt.Errorf("invalid archive path for local module %q: %w", module.Key, err)
	}
	if err := copyDirOCIWithin(module.InstallDir, targetPath, moduleCopyBoundary(module), stripPlan); err != nil {
		return fmt.Errorf("failed to copy local module %s: %w", module.Key, err)
	}
	return nil
}

func moduleInsideDownloadedPackage(tree *ResolvedTree, module *ModuleNode) bool {
	for _, pkg := range tree.Packages {
		if pkg == nil || pkg.LocalDir == "" {
			continue
		}
		rel, err := filepath.Rel(pkg.LocalDir, module.InstallDir)
		if err == nil && !relativePathEscapesRoot(rel) {
			return true
		}
	}
	return false
}

func moduleCopyBoundary(module *ModuleNode) string {
	if module.PackageRoot != "" {
		return module.PackageRoot
	}
	return module.InstallDir
}

func stagePackages(tree *ResolvedTree, packagesDir string, stripPlan *StripPlan) error {
	if len(tree.Packages) == 0 {
		return nil
	}
	if err := os.MkdirAll(packagesDir, 0o755); err != nil { //nolint:gosec // G301: standard permissions
		return fmt.Errorf("failed to create vendor directory: %w", err)
	}
	for pkgID, pkg := range tree.Packages {
		if pkg == nil || pkg.LocalDir == "" {
			return fmt.Errorf("package %q has no local directory", pkgID)
		}
		targetPath, pathErr := joinPathWithin(packagesDir, pkgID)
		if pathErr != nil {
			return fmt.Errorf("invalid package identity %q: %w", pkgID, pathErr)
		}
		if err := copyDirOCI(pkg.LocalDir, targetPath, stripPlan); err != nil {
			return fmt.Errorf("failed to copy package %s: %w", RedactSourceAddress(pkg.PackageAddr), err)
		}
	}
	return nil
}

// inPivotMode reports whether the tree is a //subdir (or implicit repo-root
// expansion) input whose archive root must pivot from the package root to the
// entry subdir (review item 2).
func inPivotMode(tree *ResolvedTree) bool {
	return tree != nil && tree.Root != nil &&
		tree.Root.PackageRoot != "" && tree.Root.PackageRoot != tree.Root.InstallDir
}

// entrySubdir returns the entry module's subdirectory relative to the package
// root (e.g. "environments/prod"), or "" when not in pivot mode.
func entrySubdir(tree *ResolvedTree) string {
	if !inPivotMode(tree) {
		return ""
	}
	rel, err := filepath.Rel(tree.Root.PackageRoot, tree.Root.InstallDir)
	if err != nil {
		return ""
	}
	return filepath.ToSlash(rel)
}

// repivotMonorepoSources rewrites local module sources in the staged entry
// that cross the new archive-root boundary in //subdir pivot mode. After this pass, every
// `../../modules/x` reference declared by the entry module that escapes the
// entry subdir is rewritten to `./<package-rel-path>` so it resolves at the
// unpacked archive root, where stageMonorepoSiblings stages the referenced
// bytes. No-op outside pivot mode. Deeper descendants' `../sibling` sources
// are NOT rewritten: stageMonorepoSiblings stages siblings at package-relative
// paths, preserving the layout and thus the relative references between them.
func repivotMonorepoSources(tree *ResolvedTree, stagedEntryDir string) error {
	if !inPivotMode(tree) {
		return nil
	}
	pkgRoot := tree.Root.PackageRoot
	entryDir := tree.Root.InstallDir

	for _, m := range tree.AllModules {
		if m == tree.Root || !m.IsLocal || m.InstallDir == "" {
			continue
		}
		// Only the entry module's DIRECT children whose targets escape the entry
		// subdir need rewriting. The entry is flattened to the archive root, so any
		// `../../modules/x` reference it declares escapes the staging dir and must
		// be repointed to the package-relative path. Deeper descendants keep their
		// original `../sibling` sources: stageMonorepoSiblings stages siblings at
		// their package-relative paths, so the layout — and thus the relative
		// references between siblings — is preserved verbatim.
		if m.Parent == nil || m.Parent.InstallDir != entryDir {
			continue
		}
		// Skip children whose target stays inside the entry subdir — staged by
		// step 1's entry copy and reachable via the original `./child` source.
		if rel, err := filepath.Rel(entryDir, m.InstallDir); err == nil && !relativePathEscapesRoot(rel) {
			continue
		}
		if err := rewriteEntryChildToPackageRel(m, pkgRoot, stagedEntryDir); err != nil {
			return err
		}
	}
	return nil
}

// rewriteEntryChildToPackageRel rewrites m's source block in the staged
// entry to `./<package-rel-path>`. Caller has already filtered m to be a
// crossing direct child of the entry in pivot mode.
func rewriteEntryChildToPackageRel(m *ModuleNode, pkgRoot, stagedEntryDir string) error {
	pkgRel, err := filepath.Rel(pkgRoot, m.InstallDir)
	if err != nil {
		return nil //nolint:nilerr // unreachable: pkgRoot is an ancestor of m.InstallDir; Rel only fails across volumes
	}
	if relativePathEscapesRoot(pkgRel) {
		return nil // target outside package root — staged by stageLocalModules
	}
	newSource := "./" + filepath.ToSlash(pkgRel)

	parentTfFiles, err := FindTerraformFiles(stagedEntryDir)
	if err != nil {
		return fmt.Errorf("failed to find parent terraform files for module %s: %w", m.Key, err)
	}
	rewrote := false
	for _, tfFile := range parentTfFiles {
		if err := RewriteModuleSource(tfFile, m.Name, newSource); err != nil {
			if errors.Is(err, ErrModuleBlockNotFound) {
				continue // this file doesn't declare the block
			}
			return fmt.Errorf("failed to rewrite source for module %s in %s: %w", m.Name, tfFile, err)
		}
		rewrote = true
	}
	if !rewrote {
		return fmt.Errorf(
			"monorepo repivot: module %q (key %s) at %s is referenced from %s but no parent .tf file declares it; "+
				"cannot rewrite its source to %s",
			m.Name, m.Key, m.InstallDir, m.Parent.InstallDir, newSource)
	}
	return nil
}

// stageMonorepoSiblings stages the parts of the package that live outside the
// entry subdir (the new archive root) but inside the package root, at their
// package-relative path. This makes the entry module's `./modules/x`-style
// references (rewritten by repivotMonorepoSources) resolve at the unpacked
// archive root. No-op outside pivot mode.
func stageMonorepoSiblings(tree *ResolvedTree, stagingDir string, stripPlan *StripPlan) error {
	if !inPivotMode(tree) {
		return nil
	}
	pkgRoot := tree.Root.PackageRoot
	entryDir := tree.Root.InstallDir

	for _, m := range tree.AllModules {
		if m == tree.Root || !m.IsLocal || m.InstallDir == "" {
			continue
		}
		// Skip modules inside the entry subdir — staged by step 1 (entry copy).
		if rel, err := filepath.Rel(entryDir, m.InstallDir); err == nil && !relativePathEscapesRoot(rel) {
			continue
		}
		// Skip modules outside the package root — staged by stageLocalModules.
		pkgRel, err := filepath.Rel(pkgRoot, m.InstallDir)
		if err != nil || relativePathEscapesRoot(pkgRel) {
			continue
		}
		if filepath.Clean(pkgRel) == "." {
			return fmt.Errorf("cannot stage package-root module %q during entry pivot; use `tofupress tree`", m.Key)
		}
		targetPath, pathErr := joinPathWithin(stagingDir, filepath.ToSlash(pkgRel))
		if pathErr != nil {
			return fmt.Errorf("invalid archive path for monorepo sibling %q: %w", m.Key, pathErr)
		}
		if err := copyDirOCIWithin(m.InstallDir, targetPath, pkgRoot, stripPlan); err != nil {
			return fmt.Errorf("failed to stage monorepo sibling %s: %w", m.Key, err)
		}
	}
	return nil
}

// aggregatePressedModulesInStaging flattens pressed local modules' nested
// vendor directories into the staging root's vendor directory. It operates
// ENTIRELY within the staging tree (never touching tree.Root.InstallDir or
// any module's InstallDir on the caller's filesystem), so the library API is
// non-destructive: a library caller passing a real on-disk tree gets it back
// byte-identical after Bundle (review item 5 / F9).
//
// Detection is read-only: pressed modules are identified by stat'ing the
// original module.InstallDir/<vDir> path. The physical move/rewrite then
// targets the analogous staged path under stagingDir, so the staged copy is
// what gets transformed, not the original.
//
//nolint:gocyclo,gocognit // complex but straightforward staging-only aggregation
func (b *Bundler) aggregatePressedModulesInStaging(tree *ResolvedTree, stagingDir string) error {
	vDir := b.vendorDir(tree)
	stagingVendorRoot := filepath.Join(stagingDir, vDir)

	// Aggregated packages land under the staging root vendor dir. Register a
	// dedicated per-package plan keyed by the staged target path so the archive
	// writer's strip-plan check keeps each aggregated package per the active
	// strip level (longest-match in PackageForPath makes the package plan win
	// over the root plan). Under full/optimistic the plan is IncludeAll (the
	// whole flattened package survives — optimistic's risk-escalation is already
	// encoded by the strip plan on the ORIGINAL paths; aggregated targets are
	// re-flattenings of bytes already kept). Under aggressive the plan is
	// config-only (.tf/.tofu), honoring ADR-0001's "aggressive: .tf/.tofu files
	// only" contract for the aggregated-vendor path (review finding G6).
	aggregatedPlans := make(map[string]*PackageStripPlan)

	for _, module := range tree.AllModules {
		if !module.IsLocal || module == tree.Root || module.InstallDir == "" {
			continue
		}

		// Detect pressed modules: read-only stat on the ORIGINAL module path.
		// Read the directory listing there so we know which pkgIDs to aggregate.
		origVendorDir := filepath.Join(module.InstallDir, vDir)
		_, statErr := os.Stat(origVendorDir)
		if os.IsNotExist(statErr) {
			continue
		} else if statErr != nil {
			return fmt.Errorf("failed to stat pressed module vendor dir %s: %w", origVendorDir, statErr)
		}

		// Compute the module's bundle path (where stageLocalModules/stageMonorepoSiblings
		// placed it under stagingDir). Same logic as those helpers: under-root modules
		// keep their package-relative path; outside-root modules use dotted Key slashes.
		moduleBundlePath := strings.ReplaceAll(module.Key, ".", "/")
		if rootRel, relErr := filepath.Rel(tree.Root.InstallDir, module.InstallDir); relErr == nil {
			if cleaned := filepath.Clean(rootRel); !relativePathEscapesRoot(cleaned) {
				moduleBundlePath = cleaned
			}
		}
		stagedModulePath := stagingDir
		if filepath.Clean(moduleBundlePath) != "." {
			var pathErr error
			stagedModulePath, pathErr = joinPathWithin(stagingDir, filepath.ToSlash(moduleBundlePath))
			if pathErr != nil {
				return fmt.Errorf("invalid staged path for pressed module %q: %w", module.Key, pathErr)
			}
		}
		stagedVendorDir := filepath.Join(stagedModulePath, vDir)

		entries, err := os.ReadDir(origVendorDir)
		if err != nil {
			return fmt.Errorf("failed to read pressed module vendor dir %s: %w", origVendorDir, err)
		}

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			packageID := entry.Name()
			stagedSourcePkgPath, sourcePathErr := joinPathWithin(stagedVendorDir, packageID)
			if sourcePathErr != nil {
				return fmt.Errorf("invalid pressed package identity %q: %w", packageID, sourcePathErr)
			}
			stagedTargetPkgPath, targetPathErr := joinPathWithin(stagingVendorRoot, packageID)
			if targetPathErr != nil {
				return fmt.Errorf("invalid pressed package identity %q: %w", packageID, targetPathErr)
			}
			origSourcePkgPath := filepath.Join(origVendorDir, packageID)

			// Skip entries that did not make it through staging (e.g. strip plan
			// excluded them). Nothing to aggregate for that pkgID.
			if _, err := os.Stat(stagedSourcePkgPath); os.IsNotExist(err) {
				continue
			}

			if err := os.MkdirAll(stagingVendorRoot, 0o755); err != nil { //nolint:gosec // G301: standard permissions
				return fmt.Errorf("failed to create staging vendor dir: %w", err)
			}

			// Aggregate to the staging root vendor dir unless a package with the
			// same ID already lives there (collected from another pressed module
			// or from the root's own resolution).
			if _, err := os.Stat(stagedTargetPkgPath); os.IsNotExist(err) {
				if err := moveOrCopyDir(stagedSourcePkgPath, stagedTargetPkgPath); err != nil {
					return fmt.Errorf("failed to aggregate package %s: %w", packageID, err)
				}

				tree.Packages[packageID] = &DownloadedPackage{
					PackageAddr: packageID,
					LocalDir:    stagedTargetPkgPath,
				}
				aggregatedPlans[filepath.Clean(stagedTargetPkgPath)] = newPackageStripPlan(stagedTargetPkgPath)
			}

			// Rewrite the staged pressed module's .tf source to point at the
			// aggregated location. Source files inside the original tree are
			// never modified (non-destructive contract).
			oldSource := "./" + vDir + "/" + packageID
			packageBundlePath := filepath.Join(vDir, packageID)
			relPath, relErr := filepath.Rel(moduleBundlePath, packageBundlePath)
			if relErr != nil {
				return fmt.Errorf("failed to calculate relative path for %s: %w", packageID, relErr)
			}
			newSource := "./" + relPath

			stagedTfFiles, err := FindTerraformFiles(stagedModulePath)
			if err != nil {
				return fmt.Errorf("failed to find terraform files in staged module %s: %w", module.Key, err)
			}
			for _, tfFile := range stagedTfFiles {
				if err := RewriteModuleSourceByOldSource(tfFile, oldSource, newSource); err != nil {
					continue // source might not be in this file
				}
			}

			// Point the in-memory tree at the staged aggregated location so any
			// downstream consumer of tree.AllModules sees the archive-visible path.
			for i, m := range tree.AllModules {
				if m.InstallDir == origSourcePkgPath {
					tree.AllModules[i].InstallDir = stagedTargetPkgPath
				}
			}
		}

		// Remove the staged nested vendor dir now that its packages are flattened
		// into the staging root vendor dir. The original caller tree is untouched.
		if err := os.RemoveAll(stagedVendorDir); err != nil {
			return fmt.Errorf("failed to remove staged nested vendor dir: %w", err)
		}
	}

	if b.StripPlan != nil {
		for _, pkgPlan := range aggregatedPlans {
			if b.StripPlan.Mode.IsAggressive() {
				// ADR-0001 aggressive: .tf/.tofu only. Populate IncludedFiles with
				// the config files present at the aggregated target so the final
				// staging walk trims non-.tf even if a future staging change copies
				// wholesale (belt-and-suspenders; stageBundle already pre-trims
				// under aggressive today).
				includeModuleConfigFiles(pkgPlan, pkgPlan.PackageRoot)
			} else {
				pkgPlan.IncludeAll = true
			}
			b.StripPlan.Packages[pkgPlan.PackageRoot] = pkgPlan
		}
	}

	return nil
}

// moveOrCopyDir relocates srcPath to dstPath. It first attempts an O(1) rename
// (same filesystem under stagingDir); if that fails it falls back to a copy +
// remove so cross-directory moves still work.
func moveOrCopyDir(srcPath, dstPath string) error {
	if err := os.Rename(srcPath, dstPath); err == nil {
		return nil
	}
	if err := copyDirOCI(srcPath, dstPath, nil); err != nil {
		return err
	}
	return os.RemoveAll(srcPath)
}

// validateVendorDir checks whether the vendor directory contains any non-package
// content that would be silently lost during bundling. When remote packages exist
// AND the vendor directory contains user-created directories, bundling would skip
// the entire vendor dir and only add back known packages, silently dropping user
// content. This check ensures we fail with a clear error instead of producing an
// inconsistent bundle.
func (b *Bundler) validateVendorDir(tree *ResolvedTree, vendorDir string) error {
	vendorPath := filepath.Join(tree.Root.InstallDir, vendorDir)

	entries, err := os.ReadDir(vendorPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no vendor dir yet, no conflict
		}
		return fmt.Errorf("failed to read vendor directory %q: %w", vendorDir, err)
	}

	if len(tree.Packages) == 0 {
		return nil // no remote packages to vendor, vendor dir skip won't trigger
	}

	// Build set of known package top-level vendor entries. With namespaced
	// vendor paths (review item 6: _vendor/namespace/name/...), the top-level
	// entry is the namespace dir, not the package dir — so compute the first
	// path component of each package's vendor-relative path.
	packageBases := make(map[string]bool, len(tree.Packages))
	for id, pkg := range tree.Packages {
		if pkg == nil || pkg.LocalDir == "" {
			return fmt.Errorf("package %q has no local directory", id)
		}
		if _, pathErr := joinPathWithin(vendorPath, id); pathErr != nil {
			return fmt.Errorf("invalid package identity %q: %w", id, pathErr)
		}
		rel, relErr := filepath.Rel(vendorPath, pkg.LocalDir)
		if relErr == nil && rel != "." && !relativePathEscapesRoot(rel) {
			first, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
			packageBases[first] = true
			continue
		}
		first, _, _ := strings.Cut(filepath.ToSlash(id), "/")
		packageBases[first] = true
	}

	// Check for non-package content that would be lost
	var conflicts []string
	for _, entry := range entries {
		if !packageBases[entry.Name()] {
			conflicts = append(conflicts, entry.Name())
		}
	}

	if len(conflicts) > 0 {
		return fmt.Errorf(
			"vendor directory %q conflicts with existing content: %v; "+
				"the vendor directory must only contain downloaded packages; "+
				"use --vendor-dir to specify a different directory name that does not conflict",
			vendorDir, conflicts,
		)
	}

	return nil
}
