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

// StripMode controls how much non-generated package content is retained.
type StripMode string

// Strip mode constants define the supported ways to filter bundle content.
const (
	StripModeNone       StripMode = "none"
	StripModeModuleDir  StripMode = "module-dir"
	StripModeConfigOnly StripMode = "config-only"
)

// ParseStripMode parses CLI/API strip mode input. Empty input means the safe default.
func ParseStripMode(raw string) (StripMode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", string(StripModeModuleDir):
		return StripModeModuleDir, nil
	case string(StripModeNone):
		return StripModeNone, nil
	case string(StripModeConfigOnly), "tf-only":
		return StripModeConfigOnly, nil
	default:
		return "", fmt.Errorf("unsupported strip mode %q (use none, module-dir, config-only, or tf-only)", raw)
	}
}

// IsAggressive reports whether this mode may remove files despite possible runtime reads.
func (m StripMode) IsAggressive() bool {
	return m == StripModeConfigOnly
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
		mode = StripModeModuleDir
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
	case StripModeNone:
		for _, pkgPlan := range plan.Packages {
			pkgPlan.IncludeAll = true
		}
	case StripModeModuleDir:
		includeResolvedModuleDirs(plan, tree)
		applyFilesystemFunctionRefs(plan, refs, true)
	case StripModeConfigOnly:
		includeConfigFilesOnly(plan, tree)
		if len(refs) > 0 {
			plan.Warnings = append(plan.Warnings, StripWarning{Message: "filesystem reads were detected; config-only stripping is aggressive and may omit runtime files"})
		}
	default:
		return nil, fmt.Errorf("unsupported strip mode: %s", mode)
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

//nolint:gocognit // subdir parent-plan inclusion adds natural complexity
func includeResolvedModuleDirs(plan *StripPlan, tree *ResolvedTree) {
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

// PackageForModuleKey finds the broadest PackageStripPlan (the package root) for a module.
func (p *StripPlan) PackageForModuleKey(moduleKey string) *PackageStripPlan {
	// Find a file from this module
	for i := range p.FilesystemFunctions {
		ref := &p.FilesystemFunctions[i]
		if ref.ModuleKey == moduleKey {
			cleaned := filepath.Clean(ref.SourceFile)
			// Return the broadest (shortest) matching package root
			var best *PackageStripPlan
			for root, pkgPlan := range p.Packages {
				if cleaned == root || strings.HasPrefix(cleaned, root+string(filepath.Separator)) {
					if best == nil || len(root) < len(best.PackageRoot) {
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
