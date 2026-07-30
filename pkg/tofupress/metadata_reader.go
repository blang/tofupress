package tofupress

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ulikunitz/xz"
)

// ReadMetadataFromDir reads metadata from an extracted artifact directory.
// It prefers the relocated .tofupress/meta.json and falls back to the legacy
// root meta.json so older extracted bundles remain readable (review item 6).
func ReadMetadataFromDir(dir string) (*ArtifactMetadata, error) {
	if dir == "" {
		return nil, fmt.Errorf("directory path is empty")
	}
	relocated := filepath.Join(dir, MetadataDir, MetadataFileName)
	data, relocatedErr := os.ReadFile(relocated) //nolint:gosec // path is constructed from user input
	if relocatedErr == nil {
		var metadata ArtifactMetadata
		if err := json.Unmarshal(data, &metadata); err != nil {
			return nil, fmt.Errorf("invalid metadata in %s: %w", relocated, err)
		}
		return &metadata, nil
	}
	if !os.IsNotExist(relocatedErr) {
		return nil, fmt.Errorf("failed to read metadata in %s: %w", relocated, relocatedErr)
	}
	metaPath := filepath.Join(dir, MetadataFileName)
	data, err := os.ReadFile(metaPath) //nolint:gosec // path is constructed from user input
	if err != nil {
		return nil, fmt.Errorf("metadata file not found in %s: %w", dir, err)
	}

	var metadata ArtifactMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, fmt.Errorf("invalid metadata in %s: %w", dir, err)
	}

	return &metadata, nil
}

// ReadMetadataFromArtifact reads metadata from a supported archive format.
func ReadMetadataFromArtifact(path string) (*ArtifactMetadata, error) {
	// Early validation: check that the file exists and is readable
	if info, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("artifact not accessible: %w", err)
	} else if info.IsDir() {
		return nil, fmt.Errorf("artifact path is a directory, not an archive: %s", path)
	}

	format, ok := DetectFormatFromPath(path)
	if !ok {
		return nil, fmt.Errorf("could not infer artifact format from file extension %q; supported: .zip, .tar.gz, .tar.xz", path)
	}

	switch format {
	case BundleFormatZIP:
		return readMetadataFromZip(path)
	case BundleFormatTarGZ:
		return readMetadataFromTarGZ(path)
	case BundleFormatTarXZ:
		return readMetadataFromTarXZ(path)
	default:
		return nil, fmt.Errorf("unsupported artifact format: %s", format)
	}
}

func readMetadataFromZip(path string) (*ArtifactMetadata, error) {
	reader, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open zip artifact: %w", err)
	}
	defer func() { _ = reader.Close() }()

	for _, file := range reader.File {
		if file.Name != MetadataRelPath {
			continue
		}
		entry, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("failed to open metadata entry: %w", err)
		}
		metadata, decErr := decodeMetadata(entry)
		_ = entry.Close()
		return metadata, decErr
	}
	// Fallback: older bundles wrote meta.json at the archive root. Accept them
	// so a v1 relocation does not break reads of pre-existing artifacts.
	for _, file := range reader.File {
		if file.Name != MetadataFileName {
			continue
		}
		entry, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("failed to open metadata entry: %w", err)
		}
		metadata, decErr := decodeMetadata(entry)
		_ = entry.Close()
		return metadata, decErr
	}
	return nil, fmt.Errorf("metadata file %s not found in artifact", MetadataRelPath)
}

func readMetadataFromTarGZ(path string) (*ArtifactMetadata, error) {
	file, err := os.Open(path) //nolint:gosec // path is provided by user
	if err != nil {
		return nil, fmt.Errorf("failed to open tar.gz artifact: %w", err)
	}
	defer func() { _ = file.Close() }()

	gzReader, err := gzip.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer func() { _ = gzReader.Close() }()

	return readMetadataFromTarReader(tar.NewReader(gzReader))
}

func readMetadataFromTarXZ(path string) (*ArtifactMetadata, error) {
	file, err := os.Open(path) //nolint:gosec // path is provided by user
	if err != nil {
		return nil, fmt.Errorf("failed to open tar.xz artifact: %w", err)
	}
	defer func() { _ = file.Close() }()

	xzReader, err := xz.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("failed to create xz reader: %w", err)
	}

	return readMetadataFromTarReader(tar.NewReader(xzReader))
}

// readMetadataFromTarReader walks a tar stream once. Because tar is a
// single-forward-pass format, we cannot peek-ahead for .tofupress/meta.json
// and then rewind; instead we record the entry bytes if we pass the legacy root
// meta.json, keep walking for the relocated path, and at EOF prefer the
// relocated entry, falling back to the legacy bytes. This preserves the
// one-pass contract while honoring both layouts (review item 6).
func readMetadataFromTarReader(reader *tar.Reader) (*ArtifactMetadata, error) {
	var legacyBytes []byte
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read tar entry: %w", err)
		}
		switch header.Name {
		case MetadataRelPath:
			return decodeMetadata(reader)
		case MetadataFileName:
			// Buffer the legacy root meta.json in case the relocated one is absent.
			b, readErr := io.ReadAll(reader)
			if readErr != nil {
				return nil, fmt.Errorf("failed to read metadata entry: %w", readErr)
			}
			legacyBytes = b
		}
	}
	if legacyBytes != nil {
		return decodeMetadata(bytes.NewReader(legacyBytes))
	}
	return nil, fmt.Errorf("metadata file %s not found in artifact", MetadataRelPath)
}

func decodeMetadata(reader io.Reader) (*ArtifactMetadata, error) {
	var metadata ArtifactMetadata
	if err := json.NewDecoder(reader).Decode(&metadata); err != nil {
		return nil, fmt.Errorf("failed to decode metadata: %w", err)
	}
	return &metadata, nil
}
