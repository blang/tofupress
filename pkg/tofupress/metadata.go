//nolint:govet // field alignment for JSON-serialized types is not critical
package tofupress

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// MetadataSchemaVersion is the current metadata JSON schema version.
const MetadataSchemaVersion = "1"

// MetadataFileName is the standard filename for metadata embedded in artifacts.
const MetadataFileName = "meta.json"

// BuildInfo records the tofupress build that produced an artifact.
type BuildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Time    string `json:"time"`
}

// BundleOptions records the options used during bundling.
type BundleOptions struct {
	Format       string `json:"format"`
	OCICompliant bool   `json:"oci_compliant"`
	StripMode    string `json:"strip_mode"`
}

// MetadataRequest contains the inputs needed to build artifact metadata.
type MetadataRequest struct {
	Build          BuildInfo
	Command        string
	Args           []string
	Options        BundleOptions
	RootSource     string
	OutputPath     string
	CreatedAt      time.Time
	StripPlan      *StripPlan              // Optional strip plan for stats and filesystem function reporting
	SourcetreePlan *SourcetreeIdentityPlan // Optional sourcetree identity plan for dedup metadata
}

// MetadataCommand records the CLI command used to create the artifact.
type MetadataCommand struct {
	Name    string        `json:"name"`
	Args    []string      `json:"args"`
	Options BundleOptions `json:"options"`
}

// MetadataArtifact describes the output artifact.
type MetadataArtifact struct {
	OutputPath   string `json:"output_path"`
	Format       string `json:"format"`
	OCICompliant bool   `json:"oci_compliant"`
	MetadataPath string `json:"metadata_path"`
	// EntrySubdir records the entry module's subdirectory relative to the
	// package root for //subdir (and implicit repo-root expansion) bundles
	// whose archive root pivots to the entry subdir (review item 2). Empty
	// for flat inputs where the archive root IS the package root.
	EntrySubdir string `json:"entry_subdir,omitempty"`
}

// ModuleMetadata records information about a single module in the resolved tree.
type ModuleMetadata struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	SourceRaw   string `json:"source_raw"`
	SourceType  string `json:"source_type"`
	PackageAddr string `json:"package_addr"`
	SubDir      string `json:"subdir"`
	InstallDir  string `json:"install_dir"`
	PackageRoot string `json:"package_root"`
	IsLocal     bool   `json:"is_local"`
	IsRemote    bool   `json:"is_remote"`
}

// PackageMetadata records information about a downloaded remote package.
type PackageMetadata struct {
	ID                   string   `json:"id"`
	PackageAddr          string   `json:"package_addr"`
	LocalDir             string   `json:"local_dir"`
	DownloadedHash       string   `json:"downloaded_hash"`
	FinalHash            string   `json:"final_hash"`
	FileCount            int      `json:"file_count"`
	SizeBytes            int64    `json:"size_bytes"`
	SourcetreeID         string   `json:"sourcetree_id,omitempty"`
	CanonicalPackageAddr string   `json:"canonical_package_addr,omitempty"`
	PackageAddrs         []string `json:"package_addrs,omitempty"`
	ModuleKeys           []string `json:"module_keys,omitempty"`
	Deduplicated         bool     `json:"deduplicated,omitempty"`
}

// DedupGroupMetadata records a group of source addresses that map to the same final package.
type DedupGroupMetadata struct {
	ID                   string   `json:"id"`
	FinalHash            string   `json:"final_hash"`
	CanonicalPackageAddr string   `json:"canonical_package_addr"`
	PackageAddrs         []string `json:"package_addrs"`
	ModuleKeys           []string `json:"module_keys"`
}

// BundleStats records aggregate statistics about the bundle.
type BundleStats struct {
	ModuleReferences     int            `json:"module_references"`
	UniquePackages       int            `json:"unique_packages"`
	LocalModules         int            `json:"local_modules"`
	RemoteModules        int            `json:"remote_modules"`
	SourceTypes          map[string]int `json:"source_types"`
	OriginalBytes        int64          `json:"original_bytes"`
	FinalBytes           int64          `json:"final_bytes"`
	StrippedBytes        int64          `json:"stripped_bytes"`
	StrippedFiles        int            `json:"stripped_files"`
	DeduplicatedPackages int            `json:"deduplicated_packages"`
}

// ArtifactMetadata is the top-level metadata structure embedded in every bundle.
type ArtifactMetadata struct {
	SchemaVersion       string                  `json:"schema_version"`
	CreatedAt           string                  `json:"created_at"`
	TofuPress           BuildInfo               `json:"tofupress"`
	Command             MetadataCommand         `json:"command"`
	Artifact            MetadataArtifact        `json:"artifact"`
	Root                ModuleMetadata          `json:"root"`
	Modules             []ModuleMetadata        `json:"modules"`
	Packages            []PackageMetadata       `json:"packages"`
	Stats               BundleStats             `json:"stats"`
	FilesystemFunctions []FilesystemFunctionRef `json:"filesystem_functions,omitempty"`
	StripWarnings       []StripWarning          `json:"strip_warnings,omitempty"`
	DedupGroups         []DedupGroupMetadata    `json:"dedup_groups,omitempty"`
}

// BuildArtifactMetadata constructs artifact metadata from a resolved module tree.
// All file paths in InstallDir and PackageRoot are made relative to the root
// module's install directory to avoid leaking temporary build paths.
func BuildArtifactMetadata(tree *ResolvedTree, req *MetadataRequest) (*ArtifactMetadata, error) {
	if tree == nil || tree.Root == nil {
		return nil, fmt.Errorf("cannot build metadata for empty resolved tree")
	}

	createdAt := req.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	modules := buildModuleMetadata(tree.AllModules, tree.Root.InstallDir)
	packages, packageBytes, err := buildPackageMetadata(tree.Packages, tree.Root.InstallDir)
	if err != nil {
		return nil, err
	}

	rootSnapshot, err := SnapshotDirectory(tree.Root.InstallDir)
	if err != nil {
		return nil, fmt.Errorf("failed to snapshot root module: %w", err)
	}

	stats := BundleStats{
		ModuleReferences: len(tree.AllModules),
		UniquePackages:   len(tree.Packages),
		LocalModules:     tree.LocalModuleCount(),
		RemoteModules:    tree.RemoteModuleCount(),
		SourceTypes:      countSourceTypes(tree.AllModules),
		OriginalBytes:    rootSnapshot.TotalBytes + packageBytes,
		FinalBytes:       rootSnapshot.TotalBytes + packageBytes,
		StrippedBytes:    0,
		StrippedFiles:    0,
	}
	if req.SourcetreePlan != nil {
		stats.DeduplicatedPackages = len(req.SourcetreePlan.DedupGroups)
	}

	// Apply strip plan stats if available
	if req.StripPlan != nil {
		stats.OriginalBytes = req.StripPlan.OriginalBytes
		stats.FinalBytes = req.StripPlan.FinalBytes
		stats.StrippedBytes = req.StripPlan.StrippedBytes
		stats.StrippedFiles = req.StripPlan.StrippedFiles
	}

	artifact := &ArtifactMetadata{
		SchemaVersion: MetadataSchemaVersion,
		CreatedAt:     createdAt.UTC().Format(time.RFC3339),
		TofuPress:     req.Build,
		Command: MetadataCommand{
			Name:    req.Command,
			Args:    safeArgs(req.Args),
			Options: req.Options,
		},
		Artifact: MetadataArtifact{
			OutputPath:   safeOutputPath(req.OutputPath),
			Format:       req.Options.Format,
			OCICompliant: req.Options.OCICompliant,
			MetadataPath: MetadataFileName,
			EntrySubdir:  entrySubdir(tree),
		},
		Root:     moduleToMetadata(tree.Root, tree.Root.InstallDir),
		Modules:  modules,
		Packages: packages,
		Stats:    stats,
	}

	if req.StripPlan != nil {
		artifact.FilesystemFunctions = req.StripPlan.FilesystemFunctions
		artifact.StripWarnings = req.StripPlan.Warnings
	}

	if req.SourcetreePlan != nil {
		artifact.DedupGroups = buildDedupGroupMetadata(req.SourcetreePlan.DedupGroups)
	}

	return artifact, nil
}

func buildModuleMetadata(nodes []*ModuleNode, rootDir string) []ModuleMetadata {
	modules := make([]ModuleMetadata, 0, len(nodes))
	for _, node := range nodes {
		modules = append(modules, moduleToMetadata(node, rootDir))
	}
	sort.Slice(modules, func(i, j int) bool {
		return modules[i].Key < modules[j].Key
	})
	return modules
}

func moduleToMetadata(node *ModuleNode, rootDir string) ModuleMetadata {
	if node == nil {
		return ModuleMetadata{}
	}
	return ModuleMetadata{
		Key:         node.Key,
		Name:        node.Name,
		SourceRaw:   node.Source.Raw,
		SourceType:  node.Source.Type.String(),
		PackageAddr: node.Source.PackageAddr,
		SubDir:      node.Source.SubDir,
		InstallDir:  relPathNoLeak(node.InstallDir, rootDir),
		PackageRoot: relPathNoLeak(node.PackageRoot, rootDir),
		IsLocal:     node.IsLocal,
		IsRemote:    node.IsRemote,
	}
}

func buildPackageMetadata(packages map[string]*DownloadedPackage, rootDir string) ([]PackageMetadata, int64, error) {
	ids := make([]string, 0, len(packages))
	for id := range packages {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	result := make([]PackageMetadata, 0, len(ids))
	var totalBytes int64
	for _, id := range ids {
		pkg := packages[id]
		snapshot, err := SnapshotDirectory(pkg.LocalDir)
		if err != nil {
			return nil, 0, fmt.Errorf("failed to snapshot package %s: %w", pkg.PackageAddr, err)
		}
		totalBytes += snapshot.TotalBytes
		result = append(result, PackageMetadata{
			ID:                   id,
			PackageAddr:          pkg.PackageAddr,
			LocalDir:             relPathNoLeak(pkg.LocalDir, rootDir),
			DownloadedHash:       pkg.ContentHash,
			FinalHash:            snapshot.Hash,
			FileCount:            snapshot.FileCount,
			SizeBytes:            snapshot.TotalBytes,
			SourcetreeID:         pkg.SourcetreeID,
			CanonicalPackageAddr: pkg.CanonicalPackageAddr,
			PackageAddrs:         append([]string(nil), pkg.PackageAddrs...),
			ModuleKeys:           append([]string(nil), pkg.ModuleKeys...),
			Deduplicated:         pkg.Deduplicated,
		})
	}
	return result, totalBytes, nil
}

func countSourceTypes(nodes []*ModuleNode) map[string]int {
	counts := make(map[string]int)
	for _, node := range nodes {
		if node == nil || node.IsRoot() {
			continue
		}
		counts[node.Source.Type.String()]++
	}
	return counts
}

// FormatSourceTypes formats a source type count map as a human-readable string.
// Returns an empty string for an empty map.
// Keys are sorted alphabetically for deterministic output.
func FormatSourceTypes(counts map[string]int) string {
	if len(counts) == 0 {
		return ""
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s: %d", k, counts[k]))
	}
	return strings.Join(parts, ", ")
}

// FormatInstallDir returns a stable display path for an install directory.
// When the path is under rootDir, it returns the relative path.
// Otherwise, it returns only the last component to avoid leaking temp paths.
func FormatInstallDir(path, rootDir string) string {
	return relPathNoLeak(path, rootDir)
}

// buildDedupGroupMetadata converts internal dedup groups to their metadata representation.
func buildDedupGroupMetadata(groups []DedupGroup) []DedupGroupMetadata {
	out := make([]DedupGroupMetadata, 0, len(groups))
	for _, group := range groups {
		out = append(out, DedupGroupMetadata{
			ID:                   group.ID,
			FinalHash:            group.FinalHash,
			CanonicalPackageAddr: group.CanonicalPackageAddr,
			PackageAddrs:         append([]string(nil), group.PackageAddrs...),
			ModuleKeys:           append([]string(nil), group.ModuleKeys...),
		})
	}
	return out
}

// relPathNoLeak returns path as a relative path under rootDir.
// When path is outside rootDir (e.g., a remote package in a separate temp
// directory), returns only the last path component to avoid leaking temporary
// build directory paths into metadata.
func relPathNoLeak(path, rootDir string) string {
	if path == "" || rootDir == "" {
		return path
	}
	rel, err := filepath.Rel(filepath.Clean(rootDir), filepath.Clean(path))
	if err != nil || strings.HasPrefix(rel, "..") {
		return filepath.Base(path)
	}
	return rel
}

// safeOutputPath makes an absolute output path relative to the current working
// directory to avoid leaking host filesystem details into metadata. If the path
// cannot be relativized, only the base filename is stored.
func safeOutputPath(p string) string {
	if !filepath.IsAbs(p) {
		return p
	}
	cwd, err := os.Getwd()
	if err != nil {
		return filepath.Base(p)
	}
	rel, err := filepath.Rel(cwd, p)
	if err != nil || strings.HasPrefix(rel, "..") {
		return filepath.Base(p)
	}
	return rel
}

// safeArgs copies args, replacing absolute paths with paths relative to CWD.
func safeArgs(args []string) []string {
	if len(args) == 0 {
		return nil
	}
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = safeOutputPath(a)
	}
	return out
}

// WriteMetadataFile writes metadata as indented JSON to the given path.
func WriteMetadataFile(path string, metadata *ArtifactMetadata) error {
	if metadata == nil {
		return fmt.Errorf("metadata is nil")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // G301: standard directory permissions for metadata output
		return fmt.Errorf("failed to create metadata directory: %w", err)
	}
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode metadata: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil { //nolint:gosec // G306: standard file permissions for metadata output
		return fmt.Errorf("failed to write metadata file: %w", err)
	}
	return nil
}
