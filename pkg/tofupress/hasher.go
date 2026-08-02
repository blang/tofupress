package tofupress

import (
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

// SnapshotDirectory computes the deterministic identity of the files staging
// will expose. Internal symlinks are dereferenced at their logical archive
// paths; external, dangling, cyclic, and special-file targets fail just as the
// staging copier does. Generated/cache directories do not affect identity.
func SnapshotDirectory(dir string) (DirectorySnapshot, error) {
	return snapshotDirectory(dir, nil)
}

func snapshotDirectory(dir string, stripPlan *StripPlan) (DirectorySnapshot, error) {
	root, err := canonicalStagingPath(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return DirectorySnapshot{}, fmt.Errorf("failed to read directory %s: directory does not exist", dir)
		}
		return DirectorySnapshot{}, fmt.Errorf("failed to read directory %s: %w", dir, err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return DirectorySnapshot{}, fmt.Errorf("failed to read directory %s: %w", dir, err)
	}
	if !info.IsDir() {
		return DirectorySnapshot{}, fmt.Errorf("failed to read directory %s: not a directory", dir)
	}

	walker := snapshotWalker{allowedRoot: root, stripPlan: stripPlan}
	if err := walker.walkDir(root, "", make(map[string]bool)); err != nil {
		return DirectorySnapshot{}, fmt.Errorf("failed to walk directory %s: %w", dir, err)
	}
	sort.Slice(walker.entries, func(i, j int) bool {
		return walker.entries[i].relPath < walker.entries[j].relPath
	})
	return hashEntries(walker.entries, walker.totalBytes), nil
}

type snapshotWalker struct {
	stripPlan   *StripPlan
	allowedRoot string
	entries     []hashEntry
	totalBytes  int64
}

func (w *snapshotWalker) walkDir(src, logicalDir string, active map[string]bool) error {
	canonical, err := canonicalStagingPath(src)
	if err != nil {
		return err
	}
	if boundaryErr := w.ensureWithinRoot(canonical); boundaryErr != nil {
		return boundaryErr
	}
	if active[canonical] {
		return fmt.Errorf("symlink cycle: directory %s is already active", canonical)
	}
	active[canonical] = true
	defer delete(active, canonical)

	entries, err := os.ReadDir(canonical)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		sourcePath := filepath.Join(canonical, entry.Name())
		logicalPath := filepath.Join(logicalDir, entry.Name())
		info, statErr := os.Lstat(sourcePath)
		if statErr != nil {
			return statErr
		}
		if shouldSkipStagingEntry(info, sourcePath, w.stripPlan) {
			continue
		}
		if err := w.walkEntry(sourcePath, logicalPath, info, active); err != nil {
			return err
		}
	}
	return nil
}

func (w *snapshotWalker) walkEntry(sourcePath, logicalPath string, info os.FileInfo, active map[string]bool) error {
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return w.walkSymlink(sourcePath, logicalPath, active)
	case info.IsDir():
		return w.walkDir(sourcePath, logicalPath, active)
	case info.Mode().IsRegular():
		return w.addFile(sourcePath, logicalPath, info.Mode())
	default:
		return fmt.Errorf("unsupported snapshot entry type %s at %s", info.Mode().Type(), sourcePath)
	}
}

func (w *snapshotWalker) walkSymlink(sourcePath, logicalPath string, active map[string]bool) error {
	resolved, err := canonicalStagingPath(sourcePath)
	if err != nil {
		return fmt.Errorf("resolve snapshot symlink %s (dangling link or symlink cycle): %w", sourcePath, err)
	}
	if boundaryErr := w.ensureWithinRoot(resolved); boundaryErr != nil {
		return boundaryErr
	}
	if isGeneratedOrVCSPath(resolved) {
		return nil
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return w.walkDir(resolved, logicalPath, active)
	}
	if info.Mode().IsRegular() {
		return w.addFile(resolved, logicalPath, info.Mode())
	}
	return fmt.Errorf("unsupported snapshot symlink target type %s at %s", info.Mode().Type(), sourcePath)
}

func (w *snapshotWalker) addFile(sourcePath, logicalPath string, mode fs.FileMode) error {
	archivePath, err := portableArchivePath(logicalPath)
	if err != nil {
		return err
	}
	content, err := os.ReadFile(sourcePath) //nolint:gosec // canonical path is package-boundary checked
	if err != nil {
		return fmt.Errorf("failed to read file %s: %w", sourcePath, err)
	}
	w.entries = append(w.entries, hashEntry{relPath: archivePath, mode: mode.Perm(), content: content})
	w.totalBytes += int64(len(content))
	return nil
}

func (w *snapshotWalker) ensureWithinRoot(resolved string) error {
	rel, err := filepath.Rel(w.allowedRoot, resolved)
	if err != nil {
		return err
	}
	if relativePathEscapesRoot(rel) {
		return fmt.Errorf("snapshot path resolves outside package root")
	}
	return nil
}

// HashModule computes a deterministic SHA-256 hash for all artifact-relevant files in dir.
func HashModule(dir string) (string, error) {
	snapshot, err := SnapshotDirectory(dir)
	if err != nil {
		return "", err
	}
	return snapshot.Hash, nil
}
