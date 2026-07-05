package tofupress

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
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
	dirNameTerraform  = ".terraform"
	dirNameGit        = ".git"
	dirNameSourceTree = "sourcetree" // legacy vendor dir name (pre-rename)
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

// NewBundler creates a new Bundler with the specified format.
func NewBundler(format BundleFormat) *Bundler {
	return &Bundler{Format: format}
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
func (b *Bundler) Bundle(tree *ResolvedTree, outputPath string) error {
	if tree == nil {
		return fmt.Errorf("tree is nil")
	}

	// Resolve the format eagerly so BundleFormatAuto never reaches the writers (F11).
	format := b.Format
	if format == BundleFormatAuto {
		detected, ok := DetectFormatFromPath(outputPath)
		if !ok {
			return fmt.Errorf("could not infer bundle format from output path %q; "+
				"pass --format=zip, --format=tar.gz, or --format=tar.xz", outputPath)
		}
		format = detected
	}
	if b.OCICompliant && format != BundleFormatZIP {
		return fmt.Errorf("--oci-compliant requires zip format (got %s)", format)
	}

	// Use the tree's vendor dir, falling back to the private default.
	b.VendorDir = vendorDirName(tree)

	// Aggregate pressed modules before creating the bundle.
	if err := b.aggregatePressedModules(tree); err != nil {
		return fmt.Errorf("failed to aggregate pressed modules: %w", err)
	}

	// Guard against a vendor directory that already contains user content; bundling
	// would otherwise silently drop that content (or mix it with vendored packages).
	if err := b.validateVendorDir(tree); err != nil {
		return err
	}

	// Repivot happens inside stageBundle so library callers that invoke it
	// directly also benefit (it is a no-op outside //subdir pivot mode).

	switch format {
	case BundleFormatZIP:
		return b.bundleZIP(tree, outputPath)
	case BundleFormatTarGZ:
		return b.bundleTarGZ(tree, outputPath)
	case BundleFormatTarXZ:
		return b.bundleTarXZ(tree, outputPath)
	default:
		return fmt.Errorf("unsupported format: %s", format)
	}
}

// rootArchiveDir returns the directory that should serve as the archive root.
//
// For //subdir package-boundary inputs (and the implicit repo-root expansion
// case, where PackageRoot is set but != InstallDir), the entry module lives at
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

// bundleTarGZ creates a tar.gz archive via staged bundling.
func (b *Bundler) bundleTarGZ(tree *ResolvedTree, outputPath string) (err error) {
	stagingDir, err := stageBundle(tree, b.VendorDir, b.StripPlan, b.Metadata)
	if err != nil {
		return err
	}
	defer func() {
		if rmErr := os.RemoveAll(stagingDir); rmErr != nil {
			err = errors.Join(err, rmErr)
		}
	}()

	return b.bundleTarGzFromDir(stagingDir, outputPath)
}

// bundleTarXZ creates a tar.xz archive via staged bundling.
func (b *Bundler) bundleTarXZ(tree *ResolvedTree, outputPath string) (err error) {
	stagingDir, err := stageBundle(tree, b.VendorDir, b.StripPlan, b.Metadata)
	if err != nil {
		return err
	}
	defer func() {
		if rmErr := os.RemoveAll(stagingDir); rmErr != nil {
			err = errors.Join(err, rmErr)
		}
	}()

	return b.bundleTarXzFromDir(stagingDir, outputPath)
}

// bundleZIP creates a ZIP archive via staged bundling.
func (b *Bundler) bundleZIP(tree *ResolvedTree, outputPath string) (err error) {
	stagingDir, err := stageBundle(tree, b.VendorDir, b.StripPlan, b.Metadata)
	if err != nil {
		return err
	}
	defer func() {
		if rmErr := os.RemoveAll(stagingDir); rmErr != nil {
			err = errors.Join(err, rmErr)
		}
	}()

	return b.bundleZipFromDir(stagingDir, outputPath)
}

// bundleZipFromDir creates a ZIP archive from a directory.
//
//nolint:gocognit,gocyclo // directory walking and zip creation is inherently complex
func (b *Bundler) bundleZipFromDir(srcDir, outputPath string) (err error) {
	outFile, err := os.Create(outputPath) //nolint:gosec // G304: path is provided by user
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

		// ZIP uses forward slashes
		archivePath := filepath.ToSlash(relPath)

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
			file, openErr := os.Open(path) //nolint:gosec // G304: path comes from our own tree
			if openErr != nil {
				return openErr
			}
			defer file.Close() //nolint:errcheck // read-only file close

			if _, copyErr := io.Copy(writer, file); copyErr != nil {
				return copyErr
			}
		}

		return nil
	})
	return //nolint:nakedret // named return needed to propagate deferred close errors
}

// bundleTarGzFromDir creates a tar.gz archive from a directory.
// Unlike the legacy bundleTarGZ, this walks the directory once
// with no vendor-dir skip logic — the directory content is assumed
// to be pre-staged.
//
//nolint:gocognit,gocyclo // directory walking and tar creation is inherently complex
func (b *Bundler) bundleTarGzFromDir(srcDir, outputPath string) (err error) {
	outFile, err := os.Create(outputPath) //nolint:gosec // G304: path is provided by user
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer func() {
		if closeErr := outFile.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()

	gzWriter := gzip.NewWriter(outFile)
	defer func() {
		if closeErr := gzWriter.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()

	tarWriter := tar.NewWriter(gzWriter)
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

		header, headerErr := tar.FileInfoHeader(info, "")
		if headerErr != nil {
			return headerErr
		}
		header.Name = relPath

		if writeErr := tarWriter.WriteHeader(header); writeErr != nil {
			return writeErr
		}

		if !info.IsDir() {
			file, openErr := os.Open(path) //nolint:gosec // G304: path comes from own tree
			if openErr != nil {
				return openErr
			}
			defer file.Close() //nolint:errcheck // read-only close

			if _, copyErr := io.Copy(tarWriter, file); copyErr != nil {
				return copyErr
			}
		}

		return nil
	})
	return //nolint:nakedret // named return needed to propagate deferred close errors
}

// bundleTarXzFromDir creates a tar.xz archive from a directory.
// Mirror of bundleTarGzFromDir using xz compression.
//
//nolint:gocognit,gocyclo // directory walking and tar/xz creation is inherently complex
func (b *Bundler) bundleTarXzFromDir(srcDir, outputPath string) (err error) {
	outFile, err := os.Create(outputPath) //nolint:gosec // G304: path is provided by user
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer func() {
		if closeErr := outFile.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()

	xzWriter, err := xz.NewWriter(outFile)
	if err != nil {
		return fmt.Errorf("failed to create xz writer: %w", err)
	}
	defer func() {
		if closeErr := xzWriter.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()

	tarWriter := tar.NewWriter(xzWriter)
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

		if info.IsDir() {
			switch info.Name() {
			case dirNameTerraform, dirNameGit:
				return filepath.SkipDir
			}
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

		header, headerErr := tar.FileInfoHeader(info, "")
		if headerErr != nil {
			return headerErr
		}
		header.Name = relPath

		if writeErr := tarWriter.WriteHeader(header); writeErr != nil {
			return writeErr
		}

		if !info.IsDir() {
			file, openErr := os.Open(path) //nolint:gosec // G304: path comes from own tree
			if openErr != nil {
				return openErr
			}
			defer file.Close() //nolint:errcheck // read-only close

			if _, copyErr := io.Copy(tarWriter, file); copyErr != nil {
				return copyErr
			}
		}

		return nil
	})
	return //nolint:nakedret // named return needed to propagate deferred close errors
}

// copyDirOCI copies a directory recursively for OCI bundling, skipping
// .terraform, .git, and sourcetree directories.
func copyDirOCI(src, dst string, stripPlan *StripPlan) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip .terraform, .git, and sourcetree directories
		if info.IsDir() {
			switch info.Name() {
			case dirNameTerraform, dirNameGit, dirNameSourceTree:
				return filepath.SkipDir
			}
		}

		if stripPlan != nil && !stripPlan.IncludePath(path, info.IsDir()) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Calculate relative path
		relPath, relErr := filepath.Rel(src, path)
		if relErr != nil {
			return relErr
		}

		targetPath := filepath.Join(dst, relPath)

		if info.IsDir() {
			return os.MkdirAll(targetPath, info.Mode()) //nolint:gosec // G301: preserve original permissions
		}

		// Copy file
		return copyFileOCI(path, targetPath, info.Mode())
	})
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

	// Repivot local sources in //subdir pivot mode BEFORE staging so the staged
	// entry's .tf files carry the rewritten `./<package-rel-path>` sources (item 2).
	// Idempotent no-op outside pivot mode.
	if err := repivotMonorepoSources(tree); err != nil {
		cleanup()
		return "", fmt.Errorf("failed to repivot monorepo sources: %w", err)
	}
	// 1. Copy root module from archive root to staging root
	archiveRoot := rootArchiveDir(tree)
	if err := copyDirOCI(archiveRoot, stagingDir, stripPlan); err != nil {
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

	// 5. Write metadata file if provided
	if metadata != nil {
		if err := WriteMetadataFile(filepath.Join(stagingDir, MetadataFileName), metadata); err != nil {
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
		// Skip remote packages (added separately from tree.Packages)
		if _, isPackage := tree.Packages[module.Source.PackageAddr]; isPackage {
			continue
		}
		if !module.IsLocal || module.InstallDir == "" {
			continue
		}
		// Skip modules inside the archive root — already copied by step 1.
		relPath, err := filepath.Rel(archiveRoot, module.InstallDir)
		if err != nil || !strings.HasPrefix(relPath, "..") {
			continue
		}
		// Skip modules inside the package root in pivot mode — staged by
		// stageMonorepoSiblings at their package-relative path.
		if tree.Root != nil && tree.Root.PackageRoot != "" && tree.Root.PackageRoot != archiveRoot {
			if relPkg, relErr := filepath.Rel(tree.Root.PackageRoot, module.InstallDir); relErr == nil && !strings.HasPrefix(relPkg, "..") {
				continue
			}
		}
		// Use module's key as the archive path to maintain structure
		archivePath := strings.ReplaceAll(module.Key, ".", "/")
		targetPath := filepath.Join(stagingDir, archivePath)
		if err := copyDirOCI(module.InstallDir, targetPath, stripPlan); err != nil {
			return fmt.Errorf("failed to copy local module %s: %w", module.Key, err)
		}
	}
	return nil
}

// stagePackages copies each downloaded package into the staging vendor directory,
// keyed by its package ID. The vendor directory is created only when packages exist.
func stagePackages(tree *ResolvedTree, packagesDir string, stripPlan *StripPlan) error {
	if len(tree.Packages) == 0 {
		return nil
	}
	if err := os.MkdirAll(packagesDir, 0o755); err != nil { //nolint:gosec // G301: standard permissions
		return fmt.Errorf("failed to create vendor directory: %w", err)
	}
	for pkgID, pkg := range tree.Packages {
		targetPath := filepath.Join(packagesDir, pkgID)
		if err := copyDirOCI(pkg.LocalDir, targetPath, stripPlan); err != nil {
			return fmt.Errorf("failed to copy package %s: %w", pkg.PackageAddr, err)
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

// repivotMonorepoSources rewrites local module sources that cross the new
// archive-root boundary in //subdir pivot mode. After this pass, every
// `../../modules/x` reference declared by the entry module that escapes the
// entry subdir is rewritten to `./<package-rel-path>` so it resolves at the
// unpacked archive root, where stageMonorepoSiblings stages the referenced
// bytes. No-op outside pivot mode. Deeper descendants' `../sibling` sources
// are NOT rewritten: stageMonorepoSiblings stages siblings at package-relative
// paths, preserving the layout and thus the relative references between them.
func repivotMonorepoSources(tree *ResolvedTree) error {
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
		if rel, err := filepath.Rel(entryDir, m.InstallDir); err == nil && !strings.HasPrefix(rel, "..") {
			continue
		}
		if err := rewriteEntryChildToPackageRel(m, pkgRoot); err != nil {
			return err
		}
	}
	return nil
}

// rewriteEntryChildToPackageRel rewrites m's parent .tf source for m to
// `./<package-rel-path>`. Caller has already filtered m to be a crossing
// direct child of the entry in pivot mode.
func rewriteEntryChildToPackageRel(m *ModuleNode, pkgRoot string) error {
	pkgRel, err := filepath.Rel(pkgRoot, m.InstallDir)
	if err != nil {
		return nil //nolint:nilerr // unreachable: pkgRoot is an ancestor of m.InstallDir; Rel only fails across volumes
	}
	if strings.HasPrefix(pkgRel, "..") {
		return nil // target outside package root — staged by stageLocalModules
	}
	newSource := "./" + filepath.ToSlash(pkgRel)

	parentTfFiles, err := FindTerraformFiles(m.Parent.InstallDir)
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
		if rel, err := filepath.Rel(entryDir, m.InstallDir); err == nil && !strings.HasPrefix(rel, "..") {
			continue
		}
		// Skip modules outside the package root — staged by stageLocalModules.
		pkgRel, err := filepath.Rel(pkgRoot, m.InstallDir)
		if err != nil || strings.HasPrefix(pkgRel, "..") {
			continue
		}
		targetPath := filepath.Join(stagingDir, filepath.FromSlash(filepath.ToSlash(pkgRel)))
		if err := copyDirOCI(m.InstallDir, targetPath, stripPlan); err != nil {
			return fmt.Errorf("failed to stage monorepo sibling %s: %w", m.Key, err)
		}
	}
	return nil
}

// aggregatePressedModules finds local modules that are pressed bundles (have their own vendor dir)
// and flattens their packages into the root vendor dir.
//
//nolint:gocyclo,gocognit // complex but straightforward aggregation logic
func (b *Bundler) aggregatePressedModules(tree *ResolvedTree) error {
	vDir := vendorDirName(tree)
	rootSourcetree := filepath.Join(tree.Root.InstallDir, vDir)

	// The strip plan was computed before aggregation (bundle.go plans stripping, then later
	// calls Bundle). Aggregated packages land under the root vendor dir, which the root
	// PackageStripPlan covers; in module-dir mode that plan keeps only the root's reached
	// config files, so stagePackages would silently drop the aggregated package. Register a
	// dedicated IncludeAll plan for each aggregated package so stagePackages copies it whole
	// (longest-match in PackageForPath makes the package plan win over the root plan).
	aggregatedPlans := make(map[string]*PackageStripPlan)

	for _, module := range tree.AllModules {
		// Skip remote modules and the root module itself
		if !module.IsLocal || module == tree.Root {
			continue
		}

		// Check if this module has its own vendor dir (it's a pressed module)
		moduleSourcetree := filepath.Join(module.InstallDir, vDir)
		if _, err := os.Stat(moduleSourcetree); os.IsNotExist(err) {
			continue
		}

		// This is a pressed module - aggregate its packages
		entries, err := os.ReadDir(moduleSourcetree)
		if err != nil {
			return fmt.Errorf("failed to read pressed module sourcetree: %w", err)
		}

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}

			packageID := entry.Name()
			sourcePkgPath := filepath.Join(moduleSourcetree, packageID)
			targetPkgPath := filepath.Join(rootSourcetree, packageID)

			// Create root sourcetree if it doesn't exist
			if err := os.MkdirAll(rootSourcetree, 0o755); err != nil { //nolint:gosec // G301: standard permissions
				return fmt.Errorf("failed to create root sourcetree: %w", err)
			}

			// Copy package to root sourcetree if it doesn't already exist
			if _, err := os.Stat(targetPkgPath); os.IsNotExist(err) {
				if err := copyDirOCI(sourcePkgPath, targetPkgPath, nil); err != nil {
					return fmt.Errorf("failed to copy package %s: %w", packageID, err)
				}

				// Add to tree.Packages
				tree.Packages[packageID] = &DownloadedPackage{
					PackageAddr: packageID,
					LocalDir:    targetPkgPath,
				}
				aggregatedPlans[filepath.Clean(targetPkgPath)] = newPackageStripPlan(targetPkgPath) // IncludeAll below
			}

			// Rewrite source in the pressed module
			oldSource := "./" + vDir + "/" + packageID
			// The new source is relative to the pressed module's *bundled* location, which
			// mirrors its location within the archive root: a pressed module that lives under
			// <root>/modules/pressed is archived at "modules/pressed" (the root dir is copied
			// wholesale so nested locals stay in place), while one outside the root is staged
			// under its dotted module.Key. Basing the rewrite on module.Key alone was wrong —
			// it produced dangling ./../_vendor/... references for pressed-in-place modules.
			moduleBundlePath := strings.ReplaceAll(module.Key, ".", "/")
			if rootRel, relErr := filepath.Rel(tree.Root.InstallDir, module.InstallDir); relErr == nil {
				if cleaned := filepath.Clean(rootRel); !strings.HasPrefix(cleaned, "..") {
					moduleBundlePath = cleaned
				}
			}
			packageBundlePath := filepath.Join(vDir, packageID)
			relPath, err := filepath.Rel(moduleBundlePath, packageBundlePath)
			if err != nil {
				return fmt.Errorf("failed to calculate relative path: %w", err)
			}
			newSource := "./" + relPath

			tfFiles, err := FindTerraformFiles(module.InstallDir)
			if err != nil {
				return fmt.Errorf("failed to find terraform files: %w", err)
			}

			for _, tfFile := range tfFiles {
				if err := RewriteModuleSourceByOldSource(tfFile, oldSource, newSource); err != nil {
					// Ignore errors - source might not be in this file
					continue
				}
			}

			// Update tree.AllModules to reflect the new location
			for i, m := range tree.AllModules {
				if m.InstallDir == sourcePkgPath {
					tree.AllModules[i].InstallDir = targetPkgPath
				}
			}
		}

		// Remove the nested sourcetree from the pressed module
		if err := os.RemoveAll(moduleSourcetree); err != nil {
			return fmt.Errorf("failed to remove nested sourcetree: %w", err)
		}
	}

	// Fold the aggregated packages into the active strip plan so stagePackages keeps them
	// whole. A nil strip plan (library callers that skip planning) is fine — stagePackages
	// then copies everything unfiltered.
	if b.StripPlan != nil {
		for _, pkgPlan := range aggregatedPlans {
			pkgPlan.IncludeAll = true
			b.StripPlan.Packages[pkgPlan.PackageRoot] = pkgPlan
		}
	}

	return nil
}

// validateVendorDir checks whether the vendor directory contains any non-package
// content that would be silently lost during bundling. When remote packages exist
// AND the vendor directory contains user-created directories, bundling would skip
// the entire vendor dir and only add back known packages, silently dropping user
// content. This check ensures we fail with a clear error instead of producing an
// inconsistent bundle.
func (b *Bundler) validateVendorDir(tree *ResolvedTree) error {
	vendorPath := filepath.Join(tree.Root.InstallDir, b.VendorDir)

	entries, err := os.ReadDir(vendorPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no vendor dir yet, no conflict
		}
		return fmt.Errorf("failed to read vendor directory %q: %w", b.VendorDir, err)
	}

	if len(tree.Packages) == 0 {
		return nil // no remote packages to vendor, vendor dir skip won't trigger
	}

	// Build set of known package directory base names
	packageBases := make(map[string]bool, len(tree.Packages))
	for _, pkg := range tree.Packages {
		packageBases[filepath.Base(pkg.LocalDir)] = true
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
			b.VendorDir, conflicts,
		)
	}

	return nil
}
