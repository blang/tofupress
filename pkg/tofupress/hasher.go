package tofupress

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// DirectorySnapshot summarizes deterministic content identity and size for a directory tree.
type DirectorySnapshot struct {
	Hash       string `json:"hash"`
	FileCount  int    `json:"file_count"`
	TotalBytes int64  `json:"total_bytes"`
}

type hashEntry struct { //nolint:govet // field alignment is not critical for this internal type
	content []byte
	relPath string
	mode    fs.FileMode
}

// SnapshotDirectory computes a deterministic SHA-256 hash and size summary of all regular files in dir.
// It skips generated/cache directories that must not affect artifact identity.
//
//nolint:gocognit,gocyclo // directory walking with safety checks requires branching
func SnapshotDirectory(dir string) (DirectorySnapshot, error) {
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return DirectorySnapshot{}, fmt.Errorf("failed to read directory %s: directory does not exist", dir)
		}
		return DirectorySnapshot{}, fmt.Errorf("failed to read directory %s: %w", dir, err)
	}
	if !info.IsDir() {
		return DirectorySnapshot{}, fmt.Errorf("failed to read directory %s: not a directory", dir)
	}

	var entries []hashEntry
	var totalBytes int64
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		name := d.Name()
		if d.IsDir() && (name == dirNameTerraform || name == dirNameGit) {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}

		fileInfo, err := d.Info()
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path) //nolint:gosec // path comes from controlled directory walk
		if err != nil {
			return fmt.Errorf("failed to read file %s: %w", path, err)
		}
		relPath, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}

		entries = append(entries, hashEntry{
			relPath: filepath.ToSlash(relPath),
			mode:    fileInfo.Mode().Perm(),
			content: content,
		})
		totalBytes += int64(len(content))
		return nil
	})
	if walkErr != nil {
		return DirectorySnapshot{}, fmt.Errorf("failed to walk directory %s: %w", dir, walkErr)
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].relPath < entries[j].relPath
	})

	h := sha256.New()
	for _, entry := range entries {
		pathRecord := fmt.Sprintf("path:%d:%s", len(entry.relPath), entry.relPath)
		modeRecord := fmt.Sprintf("mode:%04o", entry.mode)
		contentRecord := fmt.Sprintf("content:%d:", len(entry.content))
		h.Write([]byte(pathRecord))
		h.Write([]byte(modeRecord))
		h.Write([]byte(contentRecord))
		h.Write(entry.content)
	}

	return DirectorySnapshot{
		Hash:       hex.EncodeToString(h.Sum(nil)),
		FileCount:  len(entries),
		TotalBytes: totalBytes,
	}, nil
}

// HashModule computes a deterministic SHA-256 hash for all artifact-relevant files in dir.
func HashModule(dir string) (string, error) {
	snapshot, err := SnapshotDirectory(dir)
	if err != nil {
		return "", err
	}
	return snapshot.Hash, nil
}
