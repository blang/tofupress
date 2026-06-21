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

// SourcetreeIDPrefix is the standard prefix for content-addressed sourcetree package IDs.
const SourcetreeIDPrefix = "pkg-"

// PackageIdentity represents the final archive-visible identity of a resolved package.
type PackageIdentity struct {
	OldLocalDir          string
	FinalLocalDir        string
	OldID                string
	FinalID              string
	PackageAddr          string
	CanonicalPackageAddr string
	FinalHash            string
	PackageAddrs         []string
	ModuleKeys           []string
	FileCount            int
	SizeBytes            int64
	Deduplicated         bool
}

// DedupGroup records a group of source addresses that map to the same final package.
type DedupGroup struct {
	ID                   string
	FinalHash            string
	CanonicalPackageAddr string
	PackageAddrs         []string
	ModuleKeys           []string
}

// SourcetreeIdentityPlan is the complete plan for sourcetree package identities.
type SourcetreeIdentityPlan struct {
	RootSourcetreeDir string
	Packages          map[string]*PackageIdentity // keyed by old package ID
	ByFinalID         map[string]*PackageIdentity // canonical identities only
	DedupGroups       []DedupGroup
}

// SourcetreeIDFromHash creates a stable sourcetree directory ID from a SHA-256 hash.
func SourcetreeIDFromHash(hash string) (string, error) {
	if len(hash) != sha256.Size*2 {
		return "", fmt.Errorf("invalid package hash %q: expected %d lowercase hex characters", hash, sha256.Size*2)
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return "", fmt.Errorf("invalid package hash %q: %w", hash, err)
	}
	return SourcetreeIDPrefix + hash, nil
}

// SnapshotDirectoryWithStrip computes the deterministic identity of files that will be visible
// in the archive, respecting the strip plan's include/exclude decisions.
//
//nolint:gocognit,gocyclo // directory walking with strip-filtering requires branching
func SnapshotDirectoryWithStrip(dir string, stripPlan *StripPlan) (DirectorySnapshot, error) {
	if stripPlan == nil {
		return SnapshotDirectory(dir)
	}
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
		if d.IsDir() {
			if isGeneratedOrVCSPath(path) {
				return filepath.SkipDir
			}
			if !stripPlan.IncludePath(path, true) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 || !d.Type().IsRegular() {
			return nil
		}
		if !stripPlan.IncludePath(path, false) {
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
		entries = append(entries, hashEntry{relPath: filepath.ToSlash(relPath), mode: fileInfo.Mode().Perm(), content: content})
		totalBytes += int64(len(content))
		return nil
	})
	if walkErr != nil {
		return DirectorySnapshot{}, fmt.Errorf("failed to walk directory %s: %w", dir, walkErr)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].relPath < entries[j].relPath })
	return hashEntries(entries, totalBytes), nil
}

// hashEntries computes a deterministic SHA-256 hash from sorted file entries.
func hashEntries(entries []hashEntry, totalBytes int64) DirectorySnapshot {
	h := sha256.New()
	for _, entry := range entries {
		_, _ = fmt.Fprintf(h, "path:%d:%s", len(entry.relPath), entry.relPath)
		_, _ = fmt.Fprintf(h, "mode:%04o", entry.mode)
		_, _ = fmt.Fprintf(h, "content:%d:", len(entry.content))
		_, _ = h.Write(entry.content)
	}
	return DirectorySnapshot{Hash: hex.EncodeToString(h.Sum(nil)), FileCount: len(entries), TotalBytes: totalBytes}
}
