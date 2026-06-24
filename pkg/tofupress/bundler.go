package tofupress

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

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

// defaultVendorDir is the fallback vendor directory name.
const defaultVendorDir = "modules"

// vendorDirName returns the vendored modules directory name for a tree.
func vendorDirName(tree *ResolvedTree) string {
	if tree != nil && tree.VendorDir != "" {
		return tree.VendorDir
	}
	return defaultVendorDir
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

	// rootVendorDir is the absolute path of the tree's vendor directory.
	// Only this specific directory is skipped during bundling (packages added separately).
	// It prevents false-positive skips of packages that contain their own modules/ subdir.
	rootVendorDir string
}

// NewBundler creates a new Bundler with the specified format.
func NewBundler(format BundleFormat) *Bundler {
	return &Bundler{Format: format}
}

// Bundle creates an archive containing all modules in the resolved tree.
func (b *Bundler) Bundle(tree *ResolvedTree, outputPath string) error {
	if tree == nil {
		return fmt.Errorf("tree is nil")
	}

	// Use the tree's vendor dir, falling back to default
	b.VendorDir = vendorDirName(tree)

	// Pre-compute the tree root's absolute vendor directory path so that
	// addDirectoryToZip/Tar skip only this specific directory (not any
	// nested modules/ subdirectory inside a package).
	b.rootVendorDir = filepath.Join(rootArchiveDir(tree), b.VendorDir)

	// Aggregate pressed modules before creating the bundle
	if err := b.aggregatePressedModules(tree); err != nil {
		return fmt.Errorf("failed to aggregate pressed modules: %w", err)
	}

	// Use OCI-compliant mode if enabled and format is ZIP
	if b.OCICompliant && b.Format == BundleFormatZIP {
		b.VendorDir = "" // OCI inlines everything; no vendor dir to skip during zip creation
		return b.bundleOCICompliant(tree, outputPath)
	}

	// Non-OCI mode: validate vendor directory doesn't conflict with user content
	if err := b.validateVendorDir(tree); err != nil {
		return err
	}

	switch b.Format {
	case BundleFormatZIP:
		return b.bundleZIP(tree, outputPath)
	case BundleFormatTarGZ:
		return b.bundleTarGZ(tree, outputPath)
	case BundleFormatTarXZ:
		return b.bundleTarXZ(tree, outputPath)
	default:
		return fmt.Errorf("unsupported format: %s", b.Format)
	}
}

// rootArchiveDir returns the directory that should serve as the archive root.
// For //subdir package-boundary inputs, this is the PackageRoot (the full package)
// so that the subdirectory structure is preserved in the archive.
// Otherwise, it falls back to the InstallDir.
func rootArchiveDir(tree *ResolvedTree) string {
	if tree != nil && tree.Root != nil && tree.Root.PackageRoot != "" {
		return tree.Root.PackageRoot
	}
	if tree != nil && tree.Root != nil {
		return tree.Root.InstallDir
	}
	return ""
}

// bundleTarGZ creates a tar.gz archive.
//
//nolint:gocognit,gocyclo // complex but straightforward bundling logic
func (b *Bundler) bundleTarGZ(tree *ResolvedTree, outputPath string) (err error) {
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

	if err := b.addDirectoryToTar(tarWriter, rootArchiveDir(tree), "", tree.Packages); err != nil {
		return fmt.Errorf("failed to add root module: %w", err)
	}

	// Walk the tree and add all local modules that are outside the root directory
	for _, module := range tree.AllModules {
		if module == tree.Root {
			continue // Already added
		}

		// Skip remote packages (they're in tree.Packages and added separately)
		if _, isPackage := tree.Packages[module.Source.PackageAddr]; isPackage {
			continue
		}

		// For local modules, check if they're outside the root directory
		if module.IsLocal && module.InstallDir != "" {
			// Calculate relative path from root to this module
			relPath, err := filepath.Rel(rootArchiveDir(tree), module.InstallDir)
			if err != nil {
				continue
			}

			// If the module is outside the root directory (path starts with ..), add it
			if strings.HasPrefix(relPath, "..") {
				// Use the module's key as the archive path to maintain structure
				archivePath := strings.ReplaceAll(module.Key, ".", "/")
				if err := b.addDirectoryToTar(tarWriter, module.InstallDir, archivePath, tree.Packages); err != nil {
					return fmt.Errorf("failed to add module %s: %w", module.Key, err)
				}
			}
		}
	}

	for _, pkg := range tree.Packages {
		uniqueID := filepath.Base(pkg.LocalDir)
		prefix := filepath.Join(vendorDirName(tree), uniqueID)
		if err := b.addDirectoryToTar(tarWriter, pkg.LocalDir, prefix, tree.Packages); err != nil {
			return fmt.Errorf("failed to add package %s: %w", pkg.PackageAddr, err)
		}
	}

	if err := b.addMetadataToTar(tarWriter); err != nil {
		return err
	}

	return //nolint:nakedret // named return needed to propagate deferred close errors
}

// bundleTarXZ creates a tar.xz archive.
//
//nolint:gocognit,gocyclo // complex but straightforward bundling logic
func (b *Bundler) bundleTarXZ(tree *ResolvedTree, outputPath string) (err error) {
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

	if err := b.addDirectoryToTar(tarWriter, rootArchiveDir(tree), "", tree.Packages); err != nil {
		return fmt.Errorf("failed to add root module: %w", err)
	}

	// Walk the tree and add all local modules that are outside the root directory
	for _, module := range tree.AllModules {
		if module == tree.Root {
			continue // Already added
		}

		// Skip remote packages (they're in tree.Packages and added separately)
		if _, isPackage := tree.Packages[module.Source.PackageAddr]; isPackage {
			continue
		}

		// For local modules, check if they're outside the root directory
		if module.IsLocal && module.InstallDir != "" {
			// Calculate relative path from root to this module
			relPath, err := filepath.Rel(rootArchiveDir(tree), module.InstallDir)
			if err != nil {
				continue
			}

			// If the module is outside the root directory (path starts with ..), add it
			if strings.HasPrefix(relPath, "..") {
				// Use the module's key as the archive path to maintain structure
				archivePath := strings.ReplaceAll(module.Key, ".", "/")
				if err := b.addDirectoryToTar(tarWriter, module.InstallDir, archivePath, tree.Packages); err != nil {
					return fmt.Errorf("failed to add module %s: %w", module.Key, err)
				}
			}
		}
	}

	for _, pkg := range tree.Packages {
		uniqueID := filepath.Base(pkg.LocalDir)
		prefix := filepath.Join(vendorDirName(tree), uniqueID)
		if err := b.addDirectoryToTar(tarWriter, pkg.LocalDir, prefix, tree.Packages); err != nil {
			return fmt.Errorf("failed to add package %s: %w", pkg.PackageAddr, err)
		}
	}

	if err := b.addMetadataToTar(tarWriter); err != nil {
		return err
	}

	return //nolint:nakedret // named return needed to propagate deferred close errors
}

// bundleZIP creates a ZIP archive.
//
//nolint:gocognit,gocyclo // complex but straightforward bundling logic
func (b *Bundler) bundleZIP(tree *ResolvedTree, outputPath string) (err error) {
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

	if err := b.addDirectoryToZip(zipWriter, rootArchiveDir(tree), "", tree.Packages); err != nil {
		return fmt.Errorf("failed to add root module: %w", err)
	}

	// Walk the tree and add all local modules that are outside the root directory
	for _, module := range tree.AllModules {
		if module == tree.Root {
			continue // Already added
		}

		// Skip remote packages (they're in tree.Packages and added separately)
		if _, isPackage := tree.Packages[module.Source.PackageAddr]; isPackage {
			continue
		}

		// For local modules, check if they're outside the root directory
		if module.IsLocal && module.InstallDir != "" {
			// Calculate relative path from root to this module
			relPath, err := filepath.Rel(rootArchiveDir(tree), module.InstallDir)
			if err != nil {
				continue
			}

			// If the module is outside the root directory (path starts with ..), add it
			if strings.HasPrefix(relPath, "..") {
				// Use the module's key as the archive path to maintain structure
				archivePath := strings.ReplaceAll(module.Key, ".", "/")
				if err := b.addDirectoryToZip(zipWriter, module.InstallDir, archivePath, tree.Packages); err != nil {
					return fmt.Errorf("failed to add module %s: %w", module.Key, err)
				}
			}
		}
	}

	for _, pkg := range tree.Packages {
		uniqueID := filepath.Base(pkg.LocalDir)
		prefix := filepath.Join(vendorDirName(tree), uniqueID)
		if err := b.addDirectoryToZip(zipWriter, pkg.LocalDir, prefix, tree.Packages); err != nil {
			return fmt.Errorf("failed to add package %s: %w", pkg.PackageAddr, err)
		}
	}

	if err := b.addMetadataToZip(zipWriter); err != nil {
		return err
	}

	return //nolint:nakedret // named return needed to propagate deferred close errors
}

// addDirectoryToTar recursively adds a directory to a tar archive.
//
//nolint:gocyclo,gocognit // tar/zip walking with many conditionals is inherently complex
func (b *Bundler) addDirectoryToTar(tw *tar.Writer, srcDir, prefix string, packages map[string]*DownloadedPackage) error {
	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() && (info.Name() == ".terraform" || info.Name() == ".git") {
			return filepath.SkipDir
		}

		// Only skip the tree-root vendor dir when packages will be added separately.
		// Use the absolute path to avoid skipping user-created modules/ directories
		// inside downloaded packages (e.g. terraform-modules-base has modules/helper/...).
		if info.IsDir() && b.rootVendorDir != "" && path == b.rootVendorDir && len(packages) > 0 {
			return filepath.SkipDir
		}

		if b.StripPlan != nil && !b.StripPlan.IncludePath(path, info.IsDir()) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		relPath, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}

		if relPath == "." {
			return nil
		}

		archivePath := relPath
		if prefix != "" {
			archivePath = filepath.Join(prefix, relPath)
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = archivePath

		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		if !info.IsDir() {
			file, err := os.Open(path) //nolint:gosec // G304: path comes from our own tree
			if err != nil {
				return err
			}
			defer func() {
				_ = file.Close()
			}()

			if _, err := io.Copy(tw, file); err != nil {
				return err
			}
		}

		return nil
	})
}

// addDirectoryToZip recursively adds a directory to a ZIP archive.
//
//nolint:gocyclo,gocognit // zip walking with many conditionals is inherently complex
func (b *Bundler) addDirectoryToZip(zw *zip.Writer, srcDir, prefix string, packages map[string]*DownloadedPackage) error {
	err := filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() && (info.Name() == ".terraform" || info.Name() == ".git") {
			return filepath.SkipDir
		}

		// Only skip the tree-root vendor dir when packages will be added separately.
		// Use the absolute path to avoid skipping user-created modules/ directories
		// inside downloaded packages (e.g. terraform-modules-base has modules/helper/...).
		if info.IsDir() && b.rootVendorDir != "" && path == b.rootVendorDir && len(packages) > 0 {
			return filepath.SkipDir
		}

		if b.StripPlan != nil && !b.StripPlan.IncludePath(path, info.IsDir()) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		relPath, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}

		if relPath == "." {
			return nil
		}

		archivePath := relPath
		if prefix != "" {
			archivePath = filepath.Join(prefix, relPath)
		}

		// ZIP uses forward slashes for paths
		archivePath = filepath.ToSlash(archivePath)

		if info.IsDir() {
			archivePath += "/"
		}

		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = archivePath
		header.Method = zip.Deflate

		writer, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}

		if !info.IsDir() {
			file, err := os.Open(path) //nolint:gosec // G304: path comes from our own tree
			if err != nil {
				return err
			}
			defer func() {
				_ = file.Close()
			}()

			if _, err := io.Copy(writer, file); err != nil {
				return err
			}
		}

		return nil
	})
	return err
}

// bundleOCICompliant creates an OCI-compliant ZIP bundle by inlining all packages
// directly into the module tree without sourcetree metadata.
//
//nolint:gocyclo,gocognit // OCI bundling requires complex directory structure manipulation
func (b *Bundler) bundleOCICompliant(tree *ResolvedTree, outputPath string) error {
	// Create a temporary staging directory
	stagingDir, err := os.MkdirTemp("", "tofupress-oci-*")
	if err != nil {
		return fmt.Errorf("failed to create staging directory: %w", err)
	}
	defer os.RemoveAll(stagingDir) //nolint:errcheck // best-effort cleanup

	// Copy root module files to staging root
	if copyErr := copyDirOCI(tree.Root.InstallDir, stagingDir, b.StripPlan); copyErr != nil {
		return fmt.Errorf("failed to copy root module: %w", copyErr)
	}

	// Copy local modules to their relative positions
	for _, module := range tree.AllModules {
		if module == tree.Root || !module.IsLocal {
			continue
		}

		// Calculate relative path from root to this module
		relPath, relErr := filepath.Rel(tree.Root.InstallDir, module.InstallDir)
		if relErr != nil {
			continue
		}

		// If module is within root directory, it's already copied
		if !strings.HasPrefix(relPath, "..") {
			continue
		}

		// Module is outside root, copy it to staging
		// Use the relative path structure, converting ../ to modules/
		targetPath := filepath.Join(stagingDir, "modules", filepath.Base(module.InstallDir))
		if copyErr := copyDirOCI(module.InstallDir, targetPath, b.StripPlan); copyErr != nil {
			return fmt.Errorf("failed to copy local module %s: %w", module.Key, copyErr)
		}
	}

	// Copy remote packages to modules/<uniqueid>
	modulesDir := filepath.Join(stagingDir, "modules")
	if mkErr := os.MkdirAll(modulesDir, 0o755); mkErr != nil { //nolint:gosec // G301: standard permissions
		return fmt.Errorf("failed to create modules directory: %w", mkErr)
	}

	for _, pkg := range tree.Packages {
		uniqueID := filepath.Base(pkg.LocalDir)
		targetPath := filepath.Join(modulesDir, uniqueID)
		if copyErr := copyDirOCI(pkg.LocalDir, targetPath, b.StripPlan); copyErr != nil {
			return fmt.Errorf("failed to inline package %s: %w", pkg.PackageAddr, copyErr)
		}
	}

	// Rewrite all module sources to point to the new structure
	if rewriteErr := rewriteOCISources(tree, stagingDir); rewriteErr != nil {
		return fmt.Errorf("failed to rewrite sources: %w", rewriteErr)
	}

	if b.Metadata != nil {
		if err := WriteMetadataFile(filepath.Join(stagingDir, MetadataFileName), b.Metadata); err != nil {
			return fmt.Errorf("failed to stage metadata: %w", err)
		}
	}

	// Create ZIP archive from staging directory
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

		// Skip .terraform, .git, and the configured vendor directory
		if info.IsDir() {
			switch info.Name() {
			case dirNameTerraform, dirNameGit:
				return filepath.SkipDir
			}
			// Only skip the vendor dir by exact path (not just basename)
			if b.VendorDir != "" && path == filepath.Join(srcDir, b.VendorDir) {
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

	// 1. Copy root module from archive root to staging root
	archiveRoot := rootArchiveDir(tree)
	if err := copyDirOCI(archiveRoot, stagingDir, stripPlan); err != nil {
		cleanup()
		return "", fmt.Errorf("failed to copy root module to staging: %w", err)
	}

	// 2. Copy local modules that live outside the archive root
	if err := stageLocalModules(tree, archiveRoot, stagingDir, stripPlan); err != nil {
		cleanup()
		return "", err
	}

	// 3. Populate vendor directory with packages
	if err := stagePackages(tree, filepath.Join(stagingDir, vendorDir), stripPlan); err != nil {
		cleanup()
		return "", err
	}

	// 4. Write metadata file if provided
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
// are already copied by the root module walk and are skipped here.
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
		relPath, err := filepath.Rel(archiveRoot, module.InstallDir)
		if err != nil || !strings.HasPrefix(relPath, "..") {
			continue
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

// rewriteOCISources rewrites all module sources to point to the OCI-compliant structure.
//
//nolint:gocognit,unparam // source rewriting requires complex conditional logic; always returns nil by design (best-effort)
func rewriteOCISources(tree *ResolvedTree, stagingDir string) error {
	// Walk through all modules and rewrite their sources
	for _, module := range tree.AllModules {
		if module == tree.Root {
			continue
		}

		// Find the parent module's directory in staging
		var parentDir string
		switch {
		case module.Parent == tree.Root:
			parentDir = stagingDir
		case module.Parent.IsLocal:
			// Local parent: use relative path from root
			relPath, relErr := filepath.Rel(tree.Root.InstallDir, module.Parent.InstallDir)
			if relErr != nil {
				continue
			}
			if strings.HasPrefix(relPath, "..") {
				parentDir = filepath.Join(stagingDir, "modules", filepath.Base(module.Parent.InstallDir))
			} else {
				parentDir = filepath.Join(stagingDir, relPath)
			}
		default:
			// Remote parent: in modules/<uniqueid>
			parentDir = filepath.Join(stagingDir, "modules", filepath.Base(module.Parent.InstallDir))
		}

		// Find the module's target directory in staging
		var targetDir string
		if module.IsLocal {
			relPath, relErr := filepath.Rel(tree.Root.InstallDir, module.InstallDir)
			if relErr != nil {
				continue
			}
			if strings.HasPrefix(relPath, "..") {
				targetDir = filepath.Join(stagingDir, "modules", filepath.Base(module.InstallDir))
			} else {
				targetDir = filepath.Join(stagingDir, relPath)
			}
		} else {
			// Remote module: in modules/<uniqueid>
			targetDir = filepath.Join(stagingDir, "modules", filepath.Base(module.InstallDir))
		}

		// Calculate relative path from parent to target
		relPath, relErr := filepath.Rel(parentDir, targetDir)
		if relErr != nil {
			continue
		}

		newSource := "./" + relPath

		// Find and rewrite the source in the parent's .tf files
		tfFiles, findErr := FindTerraformFiles(parentDir)
		if findErr != nil {
			continue
		}

		for _, tfFile := range tfFiles {
			if rewriteErr := RewriteModuleSource(tfFile, module.Name, newSource); rewriteErr != nil {
				// Not all modules may be in all files, so ignore errors
				continue
			}
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
			}

			// Rewrite source in the pressed module
			oldSource := "./" + vDir + "/" + packageID
			// Calculate relative path based on bundle structure, not filesystem
			// In the bundle, the pressed module is at <module.Key>/ and the package is at <vendorDir>/<packageID>/
			// So we need to go up from the module to the root, then into vendorDir
			moduleBundlePath := module.Key
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

	return nil
}

func metadataJSON(metadata *ArtifactMetadata) ([]byte, error) {
	if metadata == nil {
		return nil, nil
	}
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to encode metadata: %w", err)
	}
	return append(data, '\n'), nil
}

// bundleNow returns the current time, overridable in tests.
var bundleNow = time.Now

func (b *Bundler) addMetadataToTar(tw *tar.Writer) error {
	data, err := metadataJSON(b.Metadata)
	if err != nil || data == nil {
		return err
	}
	now := bundleNow()
	header := &tar.Header{
		Name:    MetadataFileName,
		Mode:    0o644,
		Size:    int64(len(data)),
		ModTime: now,
	}
	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("failed to write metadata tar header: %w", err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("failed to write metadata tar content: %w", err)
	}
	return nil
}

func (b *Bundler) addMetadataToZip(zw *zip.Writer) error {
	data, err := metadataJSON(b.Metadata)
	if err != nil || data == nil {
		return err
	}
	now := bundleNow()
	header := &zip.FileHeader{
		Name:     MetadataFileName,
		Modified: now,
	}
	header.SetMode(0o644)
	writer, err := zw.CreateHeader(header)
	if err != nil {
		return fmt.Errorf("failed to create metadata zip entry: %w", err)
	}
	if _, err := writer.Write(data); err != nil {
		return fmt.Errorf("failed to write metadata zip content: %w", err)
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
