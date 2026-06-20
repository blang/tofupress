package tofupress

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// HashModule computes a SHA-256 hash of all .tf files in the given directory.
// It only hashes files in the root directory (not subdirectories).
// The hash is deterministic and order-independent.
func HashModule(dir string) (string, error) {
	// Check if directory exists
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("failed to read directory %s: directory does not exist", dir)
		}
		return "", fmt.Errorf("failed to read directory %s: %w", dir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("failed to read directory %s: not a directory", dir)
	}

	// Read directory entries
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("failed to read directory %s: %w", dir, err)
	}

	// Collect all .tf files
	var tfFiles []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if filepath.Ext(entry.Name()) == ".tf" {
			tfFiles = append(tfFiles, entry.Name())
		}
	}

	// Sort for deterministic ordering
	sort.Strings(tfFiles)

	// Compute hash
	h := sha256.New()
	for _, filename := range tfFiles {
		filePath := filepath.Join(dir, filename)
		f, err := os.Open(filePath)
		if err != nil {
			return "", fmt.Errorf("failed to open file %s: %w", filePath, err)
		}

		// Write filename to hash for order independence
		h.Write([]byte(filename))
		h.Write([]byte("\n"))

		// Write file content to hash
		if _, err := io.Copy(h, f); err != nil {
			f.Close()
			return "", fmt.Errorf("failed to read file %s: %w", filePath, err)
		}
		f.Close()

		// Add separator between files
		h.Write([]byte("\n"))
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}
