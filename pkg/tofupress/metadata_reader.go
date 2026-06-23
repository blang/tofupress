package tofupress

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ulikunitz/xz"
)

// ReadMetadataFromDir reads metadata from an extracted artifact directory.
// The directory should contain a meta.json file at its root.
func ReadMetadataFromDir(dir string) (*ArtifactMetadata, error) {
	if dir == "" {
		return nil, fmt.Errorf("directory path is empty")
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
	return nil, fmt.Errorf("metadata file %s not found in artifact", MetadataFileName)
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

func readMetadataFromTarReader(reader *tar.Reader) (*ArtifactMetadata, error) {
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read tar entry: %w", err)
		}
		if header.Name == MetadataFileName {
			return decodeMetadata(reader)
		}
	}
	return nil, fmt.Errorf("metadata file %s not found in artifact", MetadataFileName)
}

func decodeMetadata(reader io.Reader) (*ArtifactMetadata, error) {
	var metadata ArtifactMetadata
	if err := json.NewDecoder(reader).Decode(&metadata); err != nil {
		return nil, fmt.Errorf("failed to decode metadata: %w", err)
	}
	return &metadata, nil
}
