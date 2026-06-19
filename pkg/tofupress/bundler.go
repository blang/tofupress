package tofupress

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Bundler creates tar.gz archives from resolved module trees.
type Bundler struct{}

// NewBundler creates a new Bundler.
func NewBundler() *Bundler {
	return &Bundler{}
}

// Bundle creates a tar.gz archive containing all modules in the resolved tree.
func (b *Bundler) Bundle(tree *ResolvedTree, outputPath string) error {
	if tree == nil {
		return fmt.Errorf("tree is nil")
	}

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

	if err := b.addDirectoryToTar(tarWriter, tree.Root.InstallDir, ""); err != nil {
		return fmt.Errorf("failed to add root module: %w", err)
	}

	for _, pkg := range tree.Packages {
		uniqueID := filepath.Base(pkg.LocalDir)
		prefix := filepath.Join("sourcetree", uniqueID)
		if err := b.addDirectoryToTar(tarWriter, pkg.LocalDir, prefix); err != nil {
			return fmt.Errorf("failed to add package %s: %w", pkg.PackageAddr, err)
		}
	}

	return nil
}

// addDirectoryToTar recursively adds a directory to a tar archive.
//
//nolint:gocognit // complex but straightforward file walking logic
func (b *Bundler) addDirectoryToTar(tw *tar.Writer, srcDir, prefix string) error {
	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() && (info.Name() == ".terraform" || info.Name() == ".git") {
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
