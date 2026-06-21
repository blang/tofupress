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
	dirNameSourceTree = "sourcetree"
)

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
// Returns the detected format and true if recognized, or BundleFormatZIP and false if not.
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
		return BundleFormatZIP, false
	}
}

// Bundler creates archives from resolved module trees.
type Bundler struct {
	Format       BundleFormat
	OCICompliant bool // When true, creates OCI-compliant bundle without sourcetree/ metadata
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

	// Use OCI-compliant mode if enabled and format is ZIP
	if b.OCICompliant && b.Format == BundleFormatZIP {
		return b.bundleOCICompliant(tree, outputPath)
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

// bundleTarGZ creates a tar.gz archive.
//
//nolint:gocognit,gocyclo // complex but straightforward bundling logic
func (b *Bundler) bundleTarGZ(tree *ResolvedTree, outputPath string) error {
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

	if err := b.addDirectoryToTar(tarWriter, tree.Root.InstallDir, "", tree.Packages); err != nil {
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
			relPath, err := filepath.Rel(tree.Root.InstallDir, module.InstallDir)
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
		prefix := filepath.Join("sourcetree", uniqueID)
		if err := b.addDirectoryToTar(tarWriter, pkg.LocalDir, prefix, tree.Packages); err != nil {
			return fmt.Errorf("failed to add package %s: %w", pkg.PackageAddr, err)
		}
	}

	return nil
}

// bundleTarXZ creates a tar.xz archive.
//
//nolint:gocognit,gocyclo // complex but straightforward bundling logic
func (b *Bundler) bundleTarXZ(tree *ResolvedTree, outputPath string) error {
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

	if err := b.addDirectoryToTar(tarWriter, tree.Root.InstallDir, "", tree.Packages); err != nil {
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
			relPath, err := filepath.Rel(tree.Root.InstallDir, module.InstallDir)
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
		prefix := filepath.Join("sourcetree", uniqueID)
		if err := b.addDirectoryToTar(tarWriter, pkg.LocalDir, prefix, tree.Packages); err != nil {
			return fmt.Errorf("failed to add package %s: %w", pkg.PackageAddr, err)
		}
	}

	return nil
}

// bundleZIP creates a ZIP archive.
//
//nolint:gocognit,gocyclo // complex but straightforward bundling logic
func (b *Bundler) bundleZIP(tree *ResolvedTree, outputPath string) error {
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

	if err := b.addDirectoryToZip(zipWriter, tree.Root.InstallDir, "", tree.Packages); err != nil {
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
			relPath, err := filepath.Rel(tree.Root.InstallDir, module.InstallDir)
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
		prefix := filepath.Join("sourcetree", uniqueID)
		if err := b.addDirectoryToZip(zipWriter, pkg.LocalDir, prefix, tree.Packages); err != nil {
			return fmt.Errorf("failed to add package %s: %w", pkg.PackageAddr, err)
		}
	}

	return nil
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

		// Only skip sourcetree/ when packages will be added separately (non-empty packages map)
		// This prevents duplication in first bundle, but preserves sourcetree/ in nested bundles
		if info.IsDir() && info.Name() == "sourcetree" && len(packages) > 0 {
			return filepath.SkipDir
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

		// Only skip sourcetree/ when packages will be added separately (non-empty packages map)
		// This prevents duplication in first bundle, but preserves sourcetree/ in nested bundles
		if info.IsDir() && info.Name() == "sourcetree" && len(packages) > 0 {
			return filepath.SkipDir
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
	if copyErr := copyDirOCI(tree.Root.InstallDir, stagingDir); copyErr != nil {
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
		if copyErr := copyDirOCI(module.InstallDir, targetPath); copyErr != nil {
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
		if copyErr := copyDirOCI(pkg.LocalDir, targetPath); copyErr != nil {
			return fmt.Errorf("failed to inline package %s: %w", pkg.PackageAddr, copyErr)
		}
	}

	// Rewrite all module sources to point to the new structure
	if rewriteErr := rewriteOCISources(tree, stagingDir); rewriteErr != nil {
		return fmt.Errorf("failed to rewrite sources: %w", rewriteErr)
	}

	// Create ZIP archive from staging directory
	return b.bundleZipFromDir(stagingDir, outputPath)
}

// bundleZipFromDir creates a ZIP archive from a directory.
//
//nolint:gocognit // directory walking and zip creation is inherently complex
func (b *Bundler) bundleZipFromDir(srcDir, outputPath string) error {
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

	return filepath.Walk(srcDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		// Skip .terraform, .git, and sourcetree directories
		if info.IsDir() {
			switch info.Name() {
			case dirNameTerraform, dirNameGit, dirNameSourceTree:
				return filepath.SkipDir
			}
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
}

// copyDirOCI copies a directory recursively for OCI bundling, skipping
// .terraform, .git, and sourcetree directories.
func copyDirOCI(src, dst string) error {
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
