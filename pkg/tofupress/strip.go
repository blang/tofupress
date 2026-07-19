package tofupress

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// StripMode controls how much content is retained after the strip filter.
// See ADR-0001 for the full strip-level ladder and the anchor rule.
type StripMode string

// Strip level constants define the supported policies for filtering bundle
// content. See ADR-0001 for the strip-level ladder (full / optimistic /
// aggressive) and the anchor rule. Legacy names map via ParseStripMode aliases.
const (
	StripModeFull       StripMode = "full"       // legacy: none
	StripModeOptimistic StripMode = "optimistic" // legacy: module-dir (default)
	StripModeAggressive StripMode = "aggressive" // legacy: config-only / tf-only
)

// ParseStripMode parses CLI/API strip level input. Empty input means the
// default (optimistic). Legacy names (none, module-dir, config-only, tf-only)
// are accepted as aliases during alpha so existing tooling keeps working.
func ParseStripMode(raw string) (StripMode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", string(StripModeOptimistic), "module-dir":
		return StripModeOptimistic, nil
	case string(StripModeFull), "none":
		return StripModeFull, nil
	case string(StripModeAggressive), "config-only", "tf-only":
		return StripModeAggressive, nil
	default:
		return "", fmt.Errorf("unsupported strip level %q (use full, optimistic, or aggressive; aliases none/module-dir/config-only/tf-only)", raw)
	}
}

// IsAggressive reports whether this mode may remove files despite possible runtime reads.
func (m StripMode) IsAggressive() bool {
	return m == StripModeAggressive
}

// StripWarning is a user-visible warning produced while planning stripping.
type StripWarning struct {
	ModuleKey string `json:"module_key,omitempty"`
	Message   string `json:"message"`
}

// StrippedPath records one file excluded by the strip plan.
type StrippedPath struct {
	PackageRoot string `json:"package_root"`
	Path        string `json:"path"`
	SizeBytes   int64  `json:"size_bytes"`
}

// PackageStripPlan records include/exclude decisions for one package root.
type PackageStripPlan struct {
	IncludedDirs  map[string]struct{}
	IncludedFiles map[string]struct{}
	PackageRoot   string
	StrippedFiles []StrippedPath
	OriginalBytes int64
	FinalBytes    int64
	IncludeAll    bool
}

// StripPlan records all strip decisions for a bundle.
type StripPlan struct {
	Mode                StripMode
	Packages            map[string]*PackageStripPlan
	FilesystemFunctions []FilesystemFunctionRef
	Warnings            []StripWarning
	OriginalBytes       int64
	FinalBytes          int64
	StrippedBytes       int64
	StrippedFiles       int
}

func newPackageStripPlan(packageRoot string) *PackageStripPlan {
	return &PackageStripPlan{
		PackageRoot:   filepath.Clean(packageRoot),
		IncludedDirs:  make(map[string]struct{}),
		IncludedFiles: make(map[string]struct{}),
	}
}

func (p *PackageStripPlan) includeDir(absDir string) {
	rel := cleanRel(p.PackageRoot, absDir)
	if rel != "" {
		p.IncludedDirs[rel] = struct{}{}
	}
}

func (p *PackageStripPlan) includeFile(absFile string) {
	rel := cleanRel(p.PackageRoot, absFile)
	if rel != "" {
		p.IncludedFiles[rel] = struct{}{}
	}
}

func cleanRel(root, absPath string) string {
	rel, err := filepath.Rel(root, absPath)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return ""
	}
	return filepath.ToSlash(filepath.Clean(rel))
}

// PlanStripping builds a strip plan for a resolved module tree.
//
// The context is threaded through for API consistency and future cancellation
// hooks (review item 5); strip planning does not currently block long enough
// to honor ctx itself.
func PlanStripping(ctx context.Context, tree *ResolvedTree, mode StripMode) (*StripPlan, error) {
	_ = ctx
	if tree == nil || tree.Root == nil {
		return nil, fmt.Errorf("cannot plan stripping for empty tree")
	}
	if mode == "" {
		mode = StripModeOptimistic
	}

	plan := &StripPlan{Mode: mode, Packages: make(map[string]*PackageStripPlan)}
	for _, packageRoot := range collectPackageRoots(tree) {
		plan.Packages[filepath.Clean(packageRoot)] = newPackageStripPlan(packageRoot)
	}

	refs, err := DetectTreeFilesystemFunctions(tree)
	if err != nil {
		return nil, err
	}
	plan.FilesystemFunctions = refs

	switch mode {
	case StripModeFull:
		for _, pkgPlan := range plan.Packages {
			pkgPlan.IncludeAll = true
		}
	case StripModeOptimistic:
		// ADR-0001 optimistic keep-set:
		//   1. anchor rule — preserve every subject .tf/.tofu-bearing dir at its
		//      staged path with its config files (subject only; vendor never anchors)
		//   2. resolver-descended module dirs reachable
		//   3. static file()/fileset()/... matches with literal args
		//   4. whole owning package verbatim for any module with a risk signal
		//      (dynamic file() OR any ${path.module}/${path.root} string value)
		markAnchors(plan, tree)
		includeResolvedModuleDirs(plan, tree)
		applyFilesystemFunctionRefs(plan, refs, true)
	case StripModeAggressive:
		includeConfigFilesOnly(plan, tree)
		if len(refs) > 0 {
			plan.Warnings = append(plan.Warnings, StripWarning{Message: "filesystem reads were detected; aggressive stripping may omit runtime files"})
		}
	default:
		return nil, fmt.Errorf("unsupported strip level: %s", mode)
	}

	if err := plan.computeStats(); err != nil {
		return nil, err
	}
	return plan, nil
}

func collectPackageRoots(tree *ResolvedTree) []string {
	seen := make(map[string]struct{})
	add := func(root string) {
		if root != "" {
			seen[filepath.Clean(root)] = struct{}{}
		}
	}
	add(tree.Root.PackageRoot)
	add(tree.Root.InstallDir)
	for _, module := range tree.AllModules {
		add(module.PackageRoot)
	}
	for _, pkg := range tree.Packages {
		add(pkg.LocalDir)
	}
	roots := make([]string, 0, len(seen))
	for root := range seen {
		roots = append(roots, root)
	}
	sort.Strings(roots)
	return roots
}

//nolint:gocognit // subdir parent-plan inclusion + remote-package safety adds branching
func includeResolvedModuleDirs(plan *StripPlan, tree *ResolvedTree) {
	// After building the keep-set from resolver-descended modules, warn loudly
	// about silent content loss the keep-set cannot see: third-party downloaded
	// packages may contain `../sibling` refs inside conditional branches
	// (count=0, file()-pointed locals) the resolver never expanded. Directories
	// inside such packages that the plan would exclude are surfaced as warnings
	// so the user can re-run with --strip=full for that package. We DO NOT
	// change the strip contract here (optimistic still applies file-level
	// filtering); the safe fallback --strip=full already exists (review item 4
	// / F6 content-loss — warn-only landing).
	warnExcludedRemotePackageDirs(plan, tree)

	for _, module := range tree.AllModules {
		pkgPlan := plan.PackageForPath(module.InstallDir)
		if pkgPlan == nil {
			continue
		}
		// If the module's install dir is the package root, include config files
		// rather than the entire package (so fileset patterns can still restrict).
		if module.InstallDir == pkgPlan.PackageRoot {
			files, err := FindTerraformFiles(module.InstallDir)
			if err == nil {
				for _, file := range files {
					pkgPlan.includeFile(file)
				}
			}
		} else {
			pkgPlan.includeDir(module.InstallDir)
		}

		// Also include the module directory in any broader parent package plan
		// so the walker from the package root can reach //subdir modules.
		for _, parentPlan := range plan.Packages {
			if parentPlan == pkgPlan {
				continue
			}
			if strings.HasPrefix(module.InstallDir, parentPlan.PackageRoot+string(filepath.Separator)) {
				parentPlan.includeDir(module.InstallDir)
			}
		}
	}
}

// markAnchors implements ADR-0001's anchor rule: every directory under the
// press subject (tree.Root.InstallDir) that contains a .tf/.tofu file is an
// anchor, preserved at its staged path with its config files kept.
//
// Subject-boundary choice (ADR-0001 + ADR-0002 coherence): the anchor walk's
// subject is the entry tree (tree.Root.InstallDir), NOT the whole package
// root. Dirs outside the InstallDir subtree but inside PackageRoot (e.g.
// packageRoot/examples/, packageRoot/README.md) are NOT anchors — they stay
// governed by the keep-set, matching the existing bundle//subdir behavior.
//
// Vendor carveout: a directory inside the vendor directory is NEVER an
// anchor (vendor is not // -addressable). We skip any directory that is at
// or under a downloaded package's LocalDir so vendored .tf-dirs do not get
// anchor-kept; they are governed by the risk-detector keep-set instead.
//
// Per the corrected anchor semantics (ADR-0001, amended), the anchor rule
// guarantees LOCATION INVARIANCE + CONFIG PRESERVATION only: the dir stays
// at its staged path and its .tf/.tofu config files are kept. NON-.tf files
// in an anchor dir are NOT kept wholesale — they follow the same keep-set as
// everything else (static file()/fileset() matches and whole-owning-package
// when the module is risk-flagged). We therefore include ONLY the .tf/.tofu
// config files (via includeFile), NOT the dir itself. Walker reachability is
// preserved because includePath(dir, isDir=true) returns true when any
// IncludedFile lives under it, so the archiver descends into the anchor dir
// and reaches the .tf config without retaining the dir's non-.tf siblings.
func markAnchors(plan *StripPlan, tree *ResolvedTree) {
	subject := filepath.Clean(tree.Root.InstallDir)
	if subject == "" {
		return
	}
	vendorRoots := collectVendorRoots(tree)
	_ = filepath.WalkDir(subject, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return filepath.SkipDir // tolerate vanished paths during planning
		}
		if !d.IsDir() {
			return nil
		}
		if isGeneratedOrVCSPath(path) {
			return filepath.SkipDir
		}
		if isInsideAnyVendorRoot(path, vendorRoots) {
			return filepath.SkipDir
		}
		tfFiles, ferr := FindTerraformFiles(path)
		if ferr != nil {
			return filepath.SkipDir
		}
		if len(tfFiles) == 0 {
			return nil
		}
		pkgPlan := plan.PackageForPath(path)
		if pkgPlan == nil {
			return nil
		}
		for _, f := range tfFiles {
			pkgPlan.includeFile(f)
		}
		return nil
	})
}

// collectVendorRoots returns the cleaned LocalDir of every downloaded package
// in the tree — the set of vendor package roots that are exempt from the
// anchor rule (ADR-0001: directories inside the vendor directory are never
// anchors).
func collectVendorRoots(tree *ResolvedTree) []string {
	roots := make([]string, 0, len(tree.Packages))
	for _, pkg := range tree.Packages {
		if pkg.LocalDir != "" {
			root := filepath.Clean(pkg.LocalDir)
			roots = append(roots, root)
		}
	}
	return roots
}

// isInsideAnyVendorRoot reports whether path is at or under one of the vendor
// package roots. Used by markAnchors to apply the vendor carveout.
func isInsideAnyVendorRoot(path string, vendorRoots []string) bool {
	p := filepath.Clean(path)
	for _, root := range vendorRoots {
		if p == root || strings.HasPrefix(p, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// warnExcludedRemotePackageDirs walks each downloaded remote package's
// top-level directories and emits a warning for any directory the strip plan
// would exclude — i.e. a sibling the resolver never descended into but which a
// conditional `../sibling` reference in the third-party package could reach at
// runtime. The warning names the package and the excluded dir so the user can
// decide to re-run with --strip=full (ADR-0001).
func warnExcludedRemotePackageDirs(plan *StripPlan, tree *ResolvedTree) {
	for _, pkg := range tree.Packages {
		if pkg.LocalDir == "" {
			continue
		}
		root := filepath.Clean(pkg.LocalDir)
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			childDir := filepath.Join(root, entry.Name())
			// Skip generated/VCS directories; those are intentionally excluded.
			if isGeneratedOrVCSPath(childDir) {
				continue
			}
			if !plan.IncludePath(childDir, true) {
				plan.Warnings = append(plan.Warnings, StripWarning{
					Message: fmt.Sprintf(
						"optimistic strip level excludes directory %q inside the "+
							"downloaded remote package %s; a conditional ../%s reference "+
							"in this third-party module could reach it at runtime and fail. "+
							"Re-run with --strip=full to keep it (review item 4 / ADR-0001)",
						entry.Name(), pkg.PackageAddr, entry.Name()),
				})
			}
		}
	}
}

func includeConfigFilesOnly(plan *StripPlan, tree *ResolvedTree) {
	for _, module := range tree.AllModules {
		pkgPlan := plan.PackageForPath(module.InstallDir)
		if pkgPlan == nil {
			continue
		}
		files, err := FindTerraformFiles(module.InstallDir)
		if err != nil {
			continue
		}
		for _, file := range files {
			pkgPlan.includeFile(file)
		}
	}
}

func applyFilesystemFunctionRefs(plan *StripPlan, refs []FilesystemFunctionRef, safe bool) {
	for i := range refs {
		ref := &refs[i]
		pkgPlan := plan.PackageForPath(ref.ResolvedBase)
		if !ref.Static || pkgPlan == nil {
			modulePlan := plan.PackageForModuleKey(ref.ModuleKey)
			if safe && modulePlan != nil {
				modulePlan.IncludeAll = true
				ref.Handling = handlingDynamicFallback
			}
			continue
		}
		for _, included := range ref.IncludedPaths {
			if targetPlan := plan.PackageForPath(included); targetPlan != nil {
				targetPlan.includeFile(included)
			}
		}
	}
	plan.FilesystemFunctions = refs
}

// PackageForModuleKey finds the OWNING package plan for a module flagged with a
// risk signal. The owning package is the module's OWN (narrowest) enclosing
// package root, NOT the broadest ancestor — a vendored module keeps its own
// downloaded package, a subject module keeps its own directory subtree. This
// fixes the scope bug documented in ADR-0001 ("Owning package"), where a
// dynamic fallback previously escalated to the broadest ancestor and
// over-included the user's repo when a vendored module triggered the signal.
func (p *StripPlan) PackageForModuleKey(moduleKey string) *PackageStripPlan {
	for i := range p.FilesystemFunctions {
		ref := &p.FilesystemFunctions[i]
		if ref.ModuleKey == moduleKey {
			cleaned := filepath.Clean(ref.SourceFile)
			// Return the narrowest (longest) matching package root — the
			// module's own owning package (ADR-0001).
			var best *PackageStripPlan
			for root, pkgPlan := range p.Packages {
				if cleaned == root || strings.HasPrefix(cleaned, root+string(filepath.Separator)) {
					if best == nil || len(root) > len(best.PackageRoot) {
						best = pkgPlan
					}
				}
			}
			return best
		}
	}
	return nil
}

// PackageForPath finds the PackageStripPlan whose package root contains the given path.
func (p *StripPlan) PackageForPath(absPath string) *PackageStripPlan {
	absPath = filepath.Clean(absPath)
	var best *PackageStripPlan
	for root, pkgPlan := range p.Packages {
		if absPath == root || strings.HasPrefix(absPath, root+string(filepath.Separator)) {
			if best == nil || len(root) > len(best.PackageRoot) {
				best = pkgPlan
			}
		}
	}
	return best
}

// IncludePath reports whether the given path should be included in the archive.
func (p *StripPlan) IncludePath(absPath string, isDir bool) bool {
	pkgPlan := p.PackageForPath(absPath)
	if pkgPlan == nil {
		return true
	}
	return pkgPlan.includePath(absPath, isDir)
}

//nolint:gocognit,gocyclo // directory inclusion checks require branching
func (p *PackageStripPlan) includePath(absPath string, isDir bool) bool {
	if isGeneratedOrVCSPath(absPath) {
		return false
	}
	if p.IncludeAll {
		return true
	}
	// Always include the package root directory itself
	if absPath == p.PackageRoot {
		return true
	}
	rel := cleanRel(p.PackageRoot, absPath)
	if rel == "" {
		return false
	}
	if isDir {
		for dir := range p.IncludedDirs {
			if rel == dir || strings.HasPrefix(dir, rel+"/") || strings.HasPrefix(rel, dir+"/") {
				return true
			}
		}
		for file := range p.IncludedFiles {
			if strings.HasPrefix(file, rel+"/") {
				return true
			}
		}
		return false
	}
	if _, ok := p.IncludedFiles[rel]; ok {
		return true
	}
	for dir := range p.IncludedDirs {
		if rel == dir || strings.HasPrefix(rel, dir+"/") {
			return true
		}
	}
	return false
}

func isGeneratedOrVCSPath(absPath string) bool {
	for part := range strings.SplitSeq(filepath.ToSlash(absPath), "/") {
		switch part {
		case dirNameTerraform, dirNameGit:
			return true
		}
	}
	return false
}

//nolint:gocognit // directory walking with stats accounting requires branching
func (p *StripPlan) computeStats() error {
	for _, pkgPlan := range p.Packages {
		// Guard against directories that may have been removed during identity
		// planning (e.g., superseded package temp dirs). Skip gracefully.
		if _, statErr := os.Stat(pkgPlan.PackageRoot); statErr != nil {
			if os.IsNotExist(statErr) {
				continue
			}
			return fmt.Errorf("failed to stat package root %s: %w", pkgPlan.PackageRoot, statErr)
		}
		if err := filepath.WalkDir(pkgPlan.PackageRoot, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if isGeneratedOrVCSPath(path) {
					return filepath.SkipDir
				}
				return nil
			}
			if !entry.Type().IsRegular() {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			size := info.Size()
			pkgPlan.OriginalBytes += size
			p.OriginalBytes += size
			if pkgPlan.includePath(path, false) {
				pkgPlan.FinalBytes += size
				p.FinalBytes += size
				return nil
			}
			rel := cleanRel(pkgPlan.PackageRoot, path)
			pkgPlan.StrippedFiles = append(pkgPlan.StrippedFiles, StrippedPath{PackageRoot: pkgPlan.PackageRoot, Path: rel, SizeBytes: size})
			p.StrippedBytes += size
			p.StrippedFiles++
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}
