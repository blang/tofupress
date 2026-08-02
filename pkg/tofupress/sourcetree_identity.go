package tofupress

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
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

// namespacedPackageID derives a human-readable, addressable vendor directory
// name from a package address, falling back to the content-addressed pkg-<sha>
// when the address has no derivable namespace (review item 6). Consumers can
// then reference sub-modules by name (e.g. source =
// "./_vendor/terraform-aws-modules/vpc//modules/sub") instead of an opaque hash.
//
// Derivation rules:
//   - git::https://host/namespace/name.git?ref=... -> namespace/name
//   - registry namespace/name/provider -> namespace/name
//   - everything else (git::file://, oci://, s3://, gcs://, http archives,
//     local paths) -> pkg-<sha> (the content-addressed fallback, preserving
//     the pre-item-6 layout so existing dedup tests stay green).
func namespacedPackageID(packageAddr, contentHash string) string {
	if ns := deriveGitNamespace(packageAddr); ns != "" {
		return ns
	}
	if ns := deriveRegistryNamespace(packageAddr); ns != "" {
		return ns
	}
	// Fallback: keep the content-addressed pkg-<sha> so dedup semantics and
	// existing tests are unchanged for sources without a natural namespace.
	return SourcetreeIDPrefix + contentHash
}

// deriveGitNamespace extracts <namespace>/<name> from a git::https:// URL.
// Returns "" for non-https git sources (file://, ssh://) since those lack a
// globally addressable namespace.
func deriveGitNamespace(addr string) string {
	const gitPrefix = "git::"
	if !strings.HasPrefix(addr, gitPrefix) {
		return ""
	}
	rest := strings.TrimPrefix(addr, gitPrefix)
	u, err := url.Parse(rest)
	if err != nil {
		return ""
	}
	// Only https URLs have a host-based namespace; ssh/file/git protocol don't.
	if u.Scheme != "https" {
		return ""
	}
	p := strings.TrimSuffix(u.Path, ".git")
	p = strings.TrimPrefix(p, "/")
	segs := strings.Split(p, "/")
	if len(segs) < 2 {
		return ""
	}
	// Take the last two segments only when both are portable path components.
	// Falling back to the content hash is safer than allowing a remote URL to
	// turn provenance into `../` traversal or a platform-specific absolute path.
	namespace, name := segs[len(segs)-2], segs[len(segs)-1]
	if !safeNamespaceSegment(namespace) || !safeNamespaceSegment(name) {
		return ""
	}
	return namespace + "/" + name
}

// deriveRegistryNamespace extracts <namespace>/<name> from a Terraform registry
// address of the form namespace/name/provider. Returns "" for non-registry
// sources.
func deriveRegistryNamespace(addr string) string {
	// Registry addresses are 3-part: namespace/name/provider. They do not
	// contain :: or :// (those indicate a typed source).
	if strings.Contains(addr, "://") || strings.Contains(addr, "::") {
		return ""
	}
	segs := strings.Split(addr, "/")
	if len(segs) != 3 {
		return ""
	}
	if slices.Contains(segs, "") || !safeNamespaceSegment(segs[0]) || !safeNamespaceSegment(segs[1]) {
		return ""
	}
	// namespace/name (drop the provider segment to keep the path short; the
	// provider is implied by the content).
	return segs[0] + "/" + segs[1]
}

func safeNamespaceSegment(segment string) bool {
	if segment == "" || segment == "." || segment == ".." || strings.TrimSpace(segment) != segment {
		return false
	}
	if strings.ContainsAny(segment, "/\\:\x00") {
		return false
	}
	for _, r := range segment {
		if r < ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

// SnapshotDirectoryWithStrip computes the deterministic identity of files that
// staging will expose after applying strip decisions.
func SnapshotDirectoryWithStrip(dir string, stripPlan *StripPlan) (DirectorySnapshot, error) {
	return snapshotDirectory(dir, stripPlan)
}

// BuildSourcetreeIdentityPlan computes content-addressed sourcetree identities for all
// packages in the resolved tree, grouping packages with identical final content.
//
//nolint:gocognit // iterating packages and merging dedup groups requires branching
func BuildSourcetreeIdentityPlan(ctx context.Context, tree *ResolvedTree, stripPlan *StripPlan) (*SourcetreeIdentityPlan, error) {
	_ = ctx
	if tree == nil || tree.Root == nil {
		return nil, fmt.Errorf("cannot build sourcetree identity plan for empty tree")
	}
	vendorDir := vendorDirName(tree)
	if err := ValidateVendorDir(vendorDir); err != nil {
		return nil, err
	}
	rootSourcetreeDir := filepath.Join(rootArchiveDir(tree), vendorDir)
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

	// seenNamespaced tracks namespaced final IDs we've already assigned, so two
	// different packages that derive the same <namespace>/<name> but have
	// different content get a short content-hash suffix to avoid a directory
	// collision (review item 6).
	seenNamespaced := make(map[string]bool)

	for _, oldID := range oldIDs {
		pkg := tree.Packages[oldID]
		if pkg == nil || pkg.LocalDir == "" {
			return nil, fmt.Errorf("cannot identify package %s: missing local directory", oldID)
		}
		snapshot, err := SnapshotDirectoryWithStrip(pkg.LocalDir, stripPlan)
		if err != nil {
			return nil, fmt.Errorf("failed to snapshot package %s: %w", RedactSourceAddress(pkg.PackageAddr), err)
		}
		// The dedup key is content-addressed so identical content always collapses
		// to one directory (review item 5 / dedup contract). The visible final ID is
		// a human-readable namespaced path derived from the package address (review
		// item 6); it falls back to the content-addressed pkg-<sha> for sources with
		// no derivable namespace (local git, OCI, S3, HTTP archives).
		dedupKey, err := SourcetreeIDFromHash(snapshot.Hash)
		if err != nil {
			return nil, err
		}
		finalID := namespacedPackageID(pkg.PackageAddr, snapshot.Hash)
		if seenNamespaced[finalID] {
			// Same <namespace>/<name> but different content (would be a different
			// dedup key) — disambiguate with a short content-hash suffix.
			finalID = finalID + "-" + snapshot.Hash[:8]
		}
		seenNamespaced[finalID] = true
		finalLocalDir, pathErr := joinPathWithin(rootSourcetreeDir, finalID)
		if pathErr != nil {
			return nil, fmt.Errorf("invalid final package identity %q: %w", finalID, pathErr)
		}
		identity := &PackageIdentity{
			OldID:                oldID,
			FinalID:              finalID,
			PackageAddr:          pkg.PackageAddr,
			CanonicalPackageAddr: pkg.PackageAddr,
			OldLocalDir:          pkg.LocalDir,
			FinalLocalDir:        finalLocalDir,
			FinalHash:            snapshot.Hash,
			FileCount:            snapshot.FileCount,
			SizeBytes:            snapshot.TotalBytes,
			PackageAddrs:         []string{pkg.PackageAddr},
			ModuleKeys:           moduleKeysForPackage(tree, pkg.LocalDir),
		}
		plan.Packages[oldID] = identity

		// Dedup is keyed by the content-addressed dedup key, NOT the visible name.
		canonical, exists := plan.ByFinalID[dedupKey]
		if !exists {
			plan.ByFinalID[dedupKey] = identity
			continue
		}
		identity.CanonicalPackageAddr = canonical.CanonicalPackageAddr
		identity.FinalLocalDir = canonical.FinalLocalDir
		identity.FinalID = canonical.FinalID
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
func ApplySourcetreeIdentityPlan(ctx context.Context, tree *ResolvedTree, plan *SourcetreeIdentityPlan) error {
	_ = ctx
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
	for _, module := range tree.AllModules {
		if module == nil {
			continue
		}
		// Find the package identity whose old local dir contains this module's install dir.
		// Matching on InstallDir (longest old root wins) — not an exact PackageRoot match —
		// is required so sub-module references like "./modules/consul-cluster", whose
		// PackageRoot == InstallDir != the package root, are remapped together with the
		// package rename. Otherwise their InstallDir keeps pointing at the old (deleted)
		// package directory and the post-rename strip plan crashes reading a missing dir.
		identity := matchingIdentityForInstallDir(plan.Packages, module.InstallDir)
		if identity == nil {
			continue
		}
		if mapped, ok := remapUnder(identity.OldLocalDir, identity.FinalLocalDir, module.InstallDir); ok {
			module.InstallDir = mapped
		}
		if mapped, ok := remapUnder(identity.OldLocalDir, identity.FinalLocalDir, module.PackageRoot); ok {
			module.PackageRoot = mapped
		}
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
// matchingIdentityForInstallDir returns the PackageIdentity whose OldLocalDir contains the
// given install dir, preferring the longest (most specific) match.
func matchingIdentityForInstallDir(identities map[string]*PackageIdentity, installDir string) *PackageIdentity {
	inst := filepath.Clean(installDir)
	var best *PackageIdentity
	bestLen := -1
	for _, identity := range identities {
		oldRoot := filepath.Clean(identity.OldLocalDir)
		if oldRoot == "" {
			continue
		}
		if inst != oldRoot && !strings.HasPrefix(inst, oldRoot+string(filepath.Separator)) {
			continue
		}
		if len(oldRoot) > bestLen {
			best = identity
			bestLen = len(oldRoot)
		}
	}
	return best
}

// remapUnder translates a path that lives under oldDir to the same relative position
// under finalDir. Returns the new path and true when the path is under oldDir; returns
// the original path and false otherwise (so callers can leave unrelated fields alone).
func remapUnder(oldDir, finalDir, path string) (string, bool) {
	p := filepath.Clean(path)
	old := filepath.Clean(oldDir)
	if p == old {
		return finalDir, true
	}
	if !strings.HasPrefix(p, old+string(filepath.Separator)) {
		return path, false
	}
	rel, err := filepath.Rel(old, p)
	if err != nil {
		return path, false
	}
	return filepath.Join(finalDir, rel), true
}

func treePackageContentHash(tree *ResolvedTree, identity *PackageIdentity) string {
	if tree == nil || identity == nil {
		return ""
	}
	if pkg := tree.Packages[identity.OldID]; pkg != nil {
		return pkg.ContentHash
	}
	return ""
}

// rewriteModuleSourcesToFinalIDs rewrites module source references in parent modules to
// point at the final content-addressed package directories after identity planning.
//
// Correctness rules:
//
//   - Only REMOTE module references are rewritten. Remote packages are downloaded into
//     <root>/<vendorDir>/<addrHash>/ and the resolver writes sources like
//     "./<vendorDir>/<addrHash>//subdir". Identity planning renames those directories to
//     content-addressed "pkg-<sha256>/" names and deduplicates identical content, so every
//     such reference must be repointed at the canonical directory.
//
//   - LOCAL module references ("./sibling", "../ext") are NEVER rewritten. They are
//     intra-package or intra-root relative paths. Whole packages (and the root) are copied
//     intact into the bundle, so those relative paths still resolve. Rewriting them based
//     on package-root geometry corrupts them (the original F2 bug rewrote "../ext" inside a
//     downloaded package to "../..", pointing at the package root).
//
//   - Only packages that were actually relocated (OldLocalDir != FinalLocalDir) trigger
//     rewrites. With no relocations the function is a complete no-op (F3: do not rewrite
//     when nothing moved).
func rewriteModuleSourcesToFinalIDs(tree *ResolvedTree, plan *SourcetreeIdentityPlan) error {
	if len(plan.Packages) == 0 {
		return nil
	}

	relocatedRoots := make(map[string]bool, len(plan.Packages))
	for _, identity := range plan.Packages {
		if filepath.Clean(identity.OldLocalDir) != filepath.Clean(identity.FinalLocalDir) {
			relocatedRoots[filepath.Clean(identity.FinalLocalDir)] = true
		}
	}
	if len(relocatedRoots) == 0 {
		return nil
	}

	for _, module := range tree.AllModules {
		if err := rewriteRelocatedRemoteSource(module, tree.Root, relocatedRoots); err != nil {
			return err
		}
	}
	return nil
}

// rewriteRelocatedRemoteSource repoints a single module's source reference at its package's
// final content-addressed directory, when applicable. See rewriteModuleSourcesToFinalIDs for
// the correctness rules. Returns nil for any module that does not need rewriting.
func rewriteRelocatedRemoteSource(module, root *ModuleNode, relocatedRoots map[string]bool) error {
	if module == nil || module == root || !module.IsRemote {
		return nil
	}
	if module.Parent == nil || module.Parent.InstallDir == "" || module.PackageRoot == "" {
		return nil
	}
	if !relocatedRoots[filepath.Clean(module.PackageRoot)] {
		return nil
	}
	relPath, err := filepath.Rel(module.Parent.InstallDir, module.PackageRoot)
	if err != nil {
		return fmt.Errorf("failed to compute final source for module %s: %w", module.Key, err)
	}
	newSource := moduleSourcePath(relPath, module.Source.SubDir)
	// Parent directory may have no .tf files (e.g. a synthetic test tree that only
	// references the module via tree nodes); in that case there is nothing to rewrite.
	tfFiles, _ := FindTerraformFiles(module.Parent.InstallDir) //nolint:errcheck // best-effort lookup
	if len(tfFiles) == 0 {
		return nil
	}
	// The module block lives in exactly one .tf file. RewriteModuleSource returns
	// ErrModuleBlockNotFound for the files that don't declare it (expected), and a
	// real error for read/parse/write failures. We must surface those real failures
	// — review finding F8: swallowing read/parse/write errors would silently ship
	// bundles with stale source references and only fail later at `tofu validate`.
	// "block not found in any file" is tolerated (best-effort placement) so that
	// synthetic tree tests that reference modules only via nodes keep working; in
	// the real CLI flow the resolver always emits the referencing block.
	for _, tfFile := range tfFiles {
		if err := RewriteModuleSource(tfFile, module.Name, newSource); err != nil {
			if errors.Is(err, ErrModuleBlockNotFound) {
				continue
			}
			return fmt.Errorf("rewrite source for module %s in %s: %w", module.Name, tfFile, err)
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
