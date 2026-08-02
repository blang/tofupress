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
// ModuleKey carries the module key(s) the warning concerns, for machine-
// readable traceability in artifact metadata. For package-scoped warnings
// (e.g. an excluded sibling dir inside a downloaded package) this is the
// comma-joined set of module keys whose owning package is implicated; for
// plan-wide warnings (e.g. the aggressive "filesystem reads detected" notice)
// it is empty, since no single module is the subject. (Review nit: the field
// was previously declared but never populated.)
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

// includeDir would keep an entire directory verbatim (short of IncludeAll).
// It is currently unused by the optimistic/aggressive paths — ADR-0001 has
// them keep config files only (via includeFile) and escalate to IncludeAll
// for risk signals — but is retained as a primitive for future strip levels
// and external plan constructors. The IncludedDirs map it would populate is
// still consulted by includePath's dir-inclusion check.
//
//nolint:unused // retained primitive; see comment above
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
	if err != nil || rel == "." || relativePathEscapesRoot(rel) {
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
		//      (dynamic file() OR any ${path.module}/${path.root} string value).
		//      For ${path.root} the referenced file lives in the ROOT module's
		//      directory (not the module the ref appears in), so escalation
		//      targets the root/entry owning package — see applyFilesystemFunctionRefs.
		markAnchors(plan, tree)
		includeResolvedModuleDirs(plan, tree)
		applyFilesystemFunctionRefs(plan, refs, tree, true)
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
		// ADR-0001 optimistic keep-set: ".tf/.tofu config kept" for reachable
		// modules. We include ONLY the module's config files — never the whole
		// install dir wholesale — because the ADR's principle is uniform across
		// subject and packages: "non-.tf content is not kept wholesale"; it
		// follows the keep-set (static file()/fileset() matches, risk-escalated
		// whole owning package). The previous else-branch did includeDir
		// (wholesale) for modules whose install dir is a subdir of their
		// package root (e.g. //subdir modules, pressed-module nested vendor
		// local modules), which silently kept README/docs/examples of
		// referenced modules under optimistic — a legacy deviation from the
		// ADR keep-set that also surfaced as review finding G6 (aggregated
		// vendor packages kept verbatim regardless of strip level). Reaching
		// the module's dir from the package root still works: includePath(dir,
		// isDir=true) returns true when any IncludedFile lives under it.
		includeModuleConfigFiles(pkgPlan, module.InstallDir)

		// Also surface the module's config files in any broader parent package
		// plan so the walker from the package root can reach //subdir modules
		// (reachability only — config files, not the dir wholesale, so non-.tf
		// is still trimmed by the keep-set).
		for _, parentPlan := range plan.Packages {
			if parentPlan == pkgPlan {
				continue
			}
			if strings.HasPrefix(module.InstallDir, parentPlan.PackageRoot+string(filepath.Separator)) {
				includeModuleConfigFiles(parentPlan, module.InstallDir)
			}
		}
	}
}

// includeModuleConfigFiles adds the .tf/.tofu config files found under dir to
// pkgPlan's IncludedFiles. It is the single shared "reachable module config"
// step used by includeResolvedModuleDirs for both the install-dir-is-package-
// root case and the subdir-of-package-root case (ADR-0001: config-only, never
// wholesale).
func includeModuleConfigFiles(pkgPlan *PackageStripPlan, dir string) {
	files, err := FindTerraformFiles(dir)
	if err != nil {
		return
	}
	for _, file := range files {
		pkgPlan.includeFile(file)
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
	walkSubjectAnchors(tree, func(dir string, tfFiles []string) {
		pkgPlan := plan.PackageForPath(dir)
		if pkgPlan == nil {
			return
		}
		for _, f := range tfFiles {
			pkgPlan.includeFile(f)
		}
	})
}

// walkSubjectAnchors walks every directory under the press subject
// (tree.Root.InstallDir) and invokes visit for each directory that contains at
// least one .tf/.tofu file (an anchor), passing the OpenTofu-priority-deduped
// config file list. It uses a single os.ReadDir per directory (recursing into
// non-hidden subdirs itself) rather than filepath.WalkDir + FindTerraformFiles
// (which would ReadDir twice per dir), and applies the vendor carveout
// (directories at or under a downloaded package's LocalDir are never anchors)
// plus the standard .terraform/.git skip. Tolerates vanished paths during
// planning (best-effort, no error surfaced — strip planning is read-only).
func walkSubjectAnchors(tree *ResolvedTree, visit func(dir string, tfFiles []string)) {
	subject := filepath.Clean(tree.Root.InstallDir)
	if subject == "" {
		return
	}
	vendorRoots := collectVendorRoots(tree)
	var walk func(dir string)
	walk = func(dir string) {
		if isGeneratedOrVCSPath(dir) || isInsideAnyVendorRoot(dir, vendorRoots) {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return // tolerate vanished paths during planning
		}
		subdirs, hasTF := classifyAnchorEntries(entries, dir)
		if hasTF {
			if tfFiles, ferr := FindTerraformFiles(dir); ferr == nil {
				visit(dir, tfFiles)
			}
		}
		for _, sub := range subdirs {
			walk(sub)
		}
	}
	walk(subject)
}

// classifyAnchorEntries splits a directory's entries into the non-hidden
// subdirectory paths to recurse into and reports whether the directory itself
// is config-bearing (contains at least one non-hidden .tf/.tofu file). It is a
// single os.ReadDir-based scan used by walkSubjectAnchors so the anchor walk
// does not ReadDir twice per directory.
func classifyAnchorEntries(entries []os.DirEntry, dir string) (subdirs []string, hasTF bool) {
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if entry.IsDir() {
			subdirs = append(subdirs, filepath.Join(dir, name))
			continue
		}
		if isTerraformConfigFile(name) {
			hasTF = true
		}
	}
	return subdirs, hasTF
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
				// Attach the package's owning module key(s) so a metadata consumer
				// can link the warning back to the implicated module (review
				// nit: StripWarning.ModuleKey was previously never populated).
				keys := moduleKeysForPackage(tree, root)
				plan.Warnings = append(plan.Warnings, StripWarning{
					ModuleKey: strings.Join(keys, ","),
					Message: fmt.Sprintf(
						"optimistic strip level excludes directory %q inside the "+
							"downloaded remote package %s; a conditional ../%s reference "+
							"in this third-party module could reach it at runtime and fail. "+
							"Re-run with --strip=full to keep it (review item 4 / ADR-0001)",
						entry.Name(), RedactSourceAddress(pkg.PackageAddr), entry.Name()),
				})
			}
		}
	}
}

// includeConfigFilesOnly implements the aggressive strip level (ADR-0001):
// keep `.tf`/`.tofu` config files only; trim everything else. The contract is
// ALL config-bearing files in scope, not just resolver-reachable modules' — an
// unreferenced subject `.tf`-anchor dir (e.g. examples/big-example/main.tf) is
// still config and is kept (only its non-`.tf` siblings are trimmed). This
// matches the ADR ladder wording "aggressive: .tf/.tofu files only; trim
// everything else" and the corrected anchor reading's config-preservation
// spirit, applied uniformly to the subject tree AND every reachable (local +
// vendored) module's install dir. Risk-signaled whole-owning-package escalation
// is intentionally NOT applied — aggressive opts into the breakage risk.
func includeConfigFilesOnly(plan *StripPlan, tree *ResolvedTree) {
	// All .tf/.tofu under the press subject (referenced or not).
	markSubjectConfigFiles(plan, tree)
	// Plus reachable local/vendored modules' config (the subject walk covers the
	// entry + its subtree but not //subdir siblings or vendored packages).
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

// markSubjectConfigFiles walks the press subject (tree.Root.InstallDir) and
// includes every .tf/.tofu file it finds, mirroring markAnchors' subject walk
// (same vendor carveout and .terraform/.git skip, via walkSubjectAnchors) — but
// it is called under aggressive where EVERY subject .tf/.tofu config file is
// kept (referenced or not), while non-`.tf` content is trimmed. Walker
// reachability for the kept config is preserved because includePath(dir,
// isDir=true) returns true when any IncludedFile lives under it.
func markSubjectConfigFiles(plan *StripPlan, tree *ResolvedTree) {
	walkSubjectAnchors(tree, func(dir string, tfFiles []string) {
		pkgPlan := plan.PackageForPath(dir)
		if pkgPlan == nil {
			return
		}
		for _, f := range tfFiles {
			pkgPlan.includeFile(f)
		}
	})
}

// applyFilesystemFunctionRefs resolves the keep-set contribution of each
// detected filesystem/risk ref. Static refs with a known owning package include
// their precise matched files; non-static refs (and static refs whose resolved
// base is outside any known package) escalate an owning package to verbatim
// (IncludeAll) as the dynamic-package-fallback.
//
// G4 (ADR-0001 clarification): for a `${path.root}/...` risk signal, the
// referenced file lives in the ROOT module's directory (tree.Root.InstallDir),
// not the module the ref textually appears in. Escalating the module's own
// owning package (the narrowest root containing ref.SourceFile) would keep the
// vendor/child package verbatim but would NOT save the path.root-referenced
// file in the repo. We therefore escalate the ROOT/entry owning package for
// path.root refs, and the module's own owning package for everything else
// (path.module and dynamic file()/fileset() — the referenced file lives in
// the module's own package there).
func applyFilesystemFunctionRefs(plan *StripPlan, refs []FilesystemFunctionRef, tree *ResolvedTree, safe bool) {
	var rootPlan *PackageStripPlan
	for i := range refs {
		ref := &refs[i]
		if ref.Static {
			if pkgPlan := plan.PackageForPath(ref.ResolvedBase); pkgPlan != nil {
				for _, included := range ref.IncludedPaths {
					if targetPlan := plan.PackageForPath(included); targetPlan != nil {
						targetPlan.includeFile(included)
					}
				}
				continue
			}
		}
		// Non-static ref (or static ref whose base is outside any known package):
		// escalate an owning package to verbatim as the dynamic-package-fallback.
		if !safe {
			continue
		}
		modulePlan := owningPlanForEscalation(plan, ref, tree, &rootPlan)
		if modulePlan != nil {
			modulePlan.IncludeAll = true
			ref.Handling = handlingDynamicFallback
		}
	}
	plan.FilesystemFunctions = refs
}

// owningPlanForEscalation selects the package plan to escalate to IncludeAll
// for a non-static (risk-signaled) ref. For `${path.root}/...` refs the
// referenced file lives at the root/entry module, so escalate the root's owning
// package; otherwise escalate the module's own (narrowest) owning package.
func owningPlanForEscalation(plan *StripPlan, ref *FilesystemFunctionRef, tree *ResolvedTree, rootPlan **PackageStripPlan) *PackageStripPlan {
	if ref.Function == "path.root" && tree != nil && tree.Root != nil && tree.Root.InstallDir != "" {
		if *rootPlan == nil {
			*rootPlan = plan.PackageForPath(tree.Root.InstallDir)
		}
		if *rootPlan != nil {
			return *rootPlan
		}
	}
	return plan.PackageForModuleKey(ref.ModuleKey)
}

// PackageForModuleKey finds the OWNING package plan for a module flagged with a
// risk signal. The owning package is the module's OWN (narrowest) enclosing
// package root, NOT the broadest ancestor — a vendored module keeps its own
// downloaded package, a subject module keeps its own directory subtree. This
// fixes the scope bug documented in ADR-0001 ("Owning package"), where a
// dynamic fallback previously escalated to the broadest ancestor and
// over-included the user's repo when a vendored module triggered the signal.
//
// Equivalence note (review nit): the narrowest containing root in plan.Packages
// IS the module's owning package by construction — collectPackageRoots
// (called in PlanStripping before any escalation) registers every package
// LocalDir and every module PackageRoot, so path-containment over plan.Packages
// resolves identically to an explicit tree.Packages[addr].LocalDir lookup. The
// path-containment form is kept because PackageForModuleKey is a public
// StripPlan API with no tree handle, and because it stays correct even for
// synthetically-constructed plans in tests.
//
// Returns nil if the module key is not referenced by any FilesystemFunctionRef.
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
	p.OriginalBytes = 0
	p.FinalBytes = 0
	p.StrippedBytes = 0
	p.StrippedFiles = 0
	for _, pkgPlan := range p.Packages {
		pkgPlan.OriginalBytes = 0
		pkgPlan.FinalBytes = 0
		pkgPlan.StrippedFiles = nil
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
			// Package roots can be nested. Attribute each path only to its
			// narrowest owning plan so bytes are neither double-counted nor
			// classified by conflicting outer/inner policies.
			if owner := p.PackageForPath(path); owner != pkgPlan {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
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
