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

// BuildSourcetreeIdentityPlan computes content-addressed sourcetree identities for all
// packages in the resolved tree, grouping packages with identical final content.
//
//nolint:gocognit // iterating packages and merging dedup groups requires branching
func BuildSourcetreeIdentityPlan(tree *ResolvedTree, stripPlan *StripPlan) (*SourcetreeIdentityPlan, error) {
	if tree == nil || tree.Root == nil {
		return nil, fmt.Errorf("cannot build sourcetree identity plan for empty tree")
	}
	rootSourcetreeDir := filepath.Join(rootArchiveDir(tree), vendorDirName(tree))
	plan := &SourcetreeIdentityPlan{
		RootSourcetreeDir: rootSourcetreeDir,
		Packages:          make(map[string]*PackageIdentity),
		ByFinalID:         make(map[string]*PackageIdentity),
	}

	oldIDs := make([]string, 0, len(tree.Packages))
	for oldID := range tree.Packages {
		oldIDs = append(oldIDs, oldID)
	}
	sort.Strings(oldIDs)

	for _, oldID := range oldIDs {
		pkg := tree.Packages[oldID]
		if pkg == nil || pkg.LocalDir == "" {
			return nil, fmt.Errorf("cannot identify package %s: missing local directory", oldID)
		}
		snapshot, err := SnapshotDirectoryWithStrip(pkg.LocalDir, stripPlan)
		if err != nil {
			return nil, fmt.Errorf("failed to snapshot package %s: %w", pkg.PackageAddr, err)
		}
		finalID, err := SourcetreeIDFromHash(snapshot.Hash)
		if err != nil {
			return nil, err
		}
		identity := &PackageIdentity{
			OldID:                oldID,
			FinalID:              finalID,
			PackageAddr:          pkg.PackageAddr,
			CanonicalPackageAddr: pkg.PackageAddr,
			OldLocalDir:          pkg.LocalDir,
			FinalLocalDir:        filepath.Join(rootSourcetreeDir, finalID),
			FinalHash:            snapshot.Hash,
			FileCount:            snapshot.FileCount,
			SizeBytes:            snapshot.TotalBytes,
			PackageAddrs:         []string{pkg.PackageAddr},
			ModuleKeys:           moduleKeysForPackage(tree, pkg.LocalDir),
		}
		plan.Packages[oldID] = identity

		canonical, exists := plan.ByFinalID[finalID]
		if !exists {
			plan.ByFinalID[finalID] = identity
			continue
		}
		identity.CanonicalPackageAddr = canonical.CanonicalPackageAddr
		identity.FinalLocalDir = canonical.FinalLocalDir
		identity.Deduplicated = true
		canonical.Deduplicated = true
		canonical.PackageAddrs = appendUniqueStrings(canonical.PackageAddrs, identity.PackageAddrs...)
		canonical.ModuleKeys = appendUniqueStrings(canonical.ModuleKeys, identity.ModuleKeys...)
	}

	plan.DedupGroups = buildDedupGroups(plan.ByFinalID)
	return plan, nil
}

// moduleKeysForPackage returns all module keys whose package root matches the given directory.
func moduleKeysForPackage(tree *ResolvedTree, packageRoot string) []string {
	var keys []string
	cleanRoot := filepath.Clean(packageRoot)
	for _, module := range tree.AllModules {
		if module == nil {
			continue
		}
		if filepath.Clean(module.PackageRoot) == cleanRoot {
			keys = append(keys, module.Key)
		}
	}
	sort.Strings(keys)
	return keys
}

// appendUniqueStrings appends unique non-empty strings, preserving sorted order.
func appendUniqueStrings(values []string, extras ...string) []string {
	seen := make(map[string]struct{}, len(values)+len(extras))
	out := make([]string, 0, len(values)+len(extras))
	for _, value := range append(values, extras...) {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// buildDedupGroups extracts deduplication groups from canonical identities.
func buildDedupGroups(canonical map[string]*PackageIdentity) []DedupGroup {
	ids := make([]string, 0, len(canonical))
	for id := range canonical {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	groups := make([]DedupGroup, 0)
	for _, id := range ids {
		identity := canonical[id]
		if identity == nil || !identity.Deduplicated {
			continue
		}
		groups = append(groups, DedupGroup{
			ID:                   identity.FinalID,
			FinalHash:            identity.FinalHash,
			CanonicalPackageAddr: identity.CanonicalPackageAddr,
			PackageAddrs:         append([]string(nil), identity.PackageAddrs...),
			ModuleKeys:           append([]string(nil), identity.ModuleKeys...),
		})
	}
	return groups
}

// ApplySourcetreeIdentityPlan materializes canonical sourcetree directories, updates
// tree module pointers, rewrites module sources to final IDs, and removes superseded
// package directories.
func ApplySourcetreeIdentityPlan(tree *ResolvedTree, plan *SourcetreeIdentityPlan) error {
	if tree == nil || tree.Root == nil {
		return fmt.Errorf("cannot apply sourcetree identity plan to empty tree")
	}
	if plan == nil {
		return nil
	}
	if err := os.MkdirAll(plan.RootSourcetreeDir, 0o755); err != nil { //nolint:gosec // G301: standard directory permissions
		return fmt.Errorf("failed to create sourcetree directory: %w", err)
	}
	if err := materializeCanonicalPackages(plan); err != nil {
		return err
	}
	updateTreePackagePointers(tree, plan)
	if err := rewriteModuleSourcesToFinalIDs(tree, plan); err != nil {
		return err
	}
	if err := removeSupersededPackageDirs(plan); err != nil {
		return err
	}
	return nil
}

// materializeCanonicalPackages copies canonical packages to their final sourcetree locations.
func materializeCanonicalPackages(plan *SourcetreeIdentityPlan) error {
	ids := make([]string, 0, len(plan.ByFinalID))
	for id := range plan.ByFinalID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		identity := plan.ByFinalID[id]
		if filepath.Clean(identity.OldLocalDir) == filepath.Clean(identity.FinalLocalDir) {
			continue
		}
		if err := os.RemoveAll(identity.FinalLocalDir); err != nil {
			return fmt.Errorf("failed to clear final package directory %s: %w", identity.FinalLocalDir, err)
		}
		if err := copyDirOCI(identity.OldLocalDir, identity.FinalLocalDir, nil); err != nil {
			return fmt.Errorf("failed to materialize final package %s: %w", identity.FinalID, err)
		}
	}
	return nil
}

// updateTreePackagePointers updates module PackageRoot and InstallDir fields to point to
// the final sourcetree directories, and rebuilds tree.Packages with final identities.
func updateTreePackagePointers(tree *ResolvedTree, plan *SourcetreeIdentityPlan) {
	oldRootToIdentity := make(map[string]*PackageIdentity)
	for _, identity := range plan.Packages {
		oldRootToIdentity[filepath.Clean(identity.OldLocalDir)] = identity
	}
	for _, module := range tree.AllModules {
		if module == nil {
			continue
		}
		identity := oldRootToIdentity[filepath.Clean(module.PackageRoot)]
		if identity == nil {
			continue
		}
		rel, err := filepath.Rel(identity.OldLocalDir, module.InstallDir)
		if err != nil || rel == "." {
			rel = ""
		}
		module.PackageRoot = identity.FinalLocalDir
		module.InstallDir = filepath.Join(identity.FinalLocalDir, rel)
	}

	finalPackages := make(map[string]*DownloadedPackage, len(plan.ByFinalID))
	for id, identity := range plan.ByFinalID {
		finalPackages[id] = &DownloadedPackage{
			PackageAddr:          identity.CanonicalPackageAddr,
			LocalDir:             identity.FinalLocalDir,
			ContentHash:          treePackageContentHash(tree, identity),
			SourcetreeID:         identity.FinalID,
			FinalHash:            identity.FinalHash,
			CanonicalPackageAddr: identity.CanonicalPackageAddr,
			PackageAddrs:         append([]string(nil), identity.PackageAddrs...),
			ModuleKeys:           append([]string(nil), identity.ModuleKeys...),
			Deduplicated:         identity.Deduplicated,
		}
	}
	tree.Packages = finalPackages
}

// treePackageContentHash looks up the original download hash for an identity.
func treePackageContentHash(tree *ResolvedTree, identity *PackageIdentity) string {
	if tree == nil || identity == nil {
		return ""
	}
	if pkg := tree.Packages[identity.OldID]; pkg != nil {
		return pkg.ContentHash
	}
	return ""
}

// rewriteModuleSourcesToFinalIDs rewrites module source references in parent modules
// to point to the final sourcetree IDs. Only modules whose package root was affected
// by the identity plan are rewritten; local modules within the root package are left alone.
func rewriteModuleSourcesToFinalIDs(tree *ResolvedTree, plan *SourcetreeIdentityPlan) error {
	if len(plan.Packages) == 0 {
		return nil
	}
	// Build a set of final package roots that were affected by the identity plan.
	// Use FinalLocalDir (post-update) because updateTreePackagePointers already
	// updated module.PackageRoot to the canonical location.
	affectedRoots := make(map[string]bool)
	for _, identity := range plan.ByFinalID {
		affectedRoots[filepath.Clean(identity.FinalLocalDir)] = true
	}
	for _, module := range tree.AllModules {
		if module == nil || module.Parent == nil || module.Parent.InstallDir == "" || module.PackageRoot == "" {
			continue
		}
		// Only rewrite sources for modules whose package was actually moved by identity planning
		if !affectedRoots[filepath.Clean(module.PackageRoot)] {
			continue
		}
		relPath, err := filepath.Rel(module.Parent.InstallDir, module.PackageRoot)
		if err != nil {
			return fmt.Errorf("failed to compute final source for module %s: %w", module.Key, err)
		}
		newSource := moduleSourcePath(relPath, module.Source.SubDir)
		tfFiles, err := FindTerraformFiles(module.Parent.InstallDir)
		if err != nil {
			return fmt.Errorf("failed to scan parent module %s for source rewrite: %w", module.Parent.Key, err)
		}
		for _, tfFile := range tfFiles {
			// Not all modules may be in every .tf file (e.g. outputs.tf)
			_ = RewriteModuleSource(tfFile, module.Name, newSource)
		}
	}
	return nil
}

// removeSupersededPackageDirs removes old package directories that have been replaced by
// canonical sourcetree directories.
func removeSupersededPackageDirs(plan *SourcetreeIdentityPlan) error {
	for _, identity := range plan.Packages {
		if filepath.Clean(identity.OldLocalDir) == filepath.Clean(identity.FinalLocalDir) {
			continue
		}
		if err := os.RemoveAll(identity.OldLocalDir); err != nil {
			return fmt.Errorf("failed to remove superseded package directory %s: %w", identity.OldLocalDir, err)
		}
	}
	return nil
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
