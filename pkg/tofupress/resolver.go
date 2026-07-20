package tofupress

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// downloadRequest represents a pending package download.
type downloadRequest struct {
	packageAddr string
	localPath   string
	uniqueID    string
}

// downloadResult represents the result of a package download.
type downloadResult struct {
	request     downloadRequest
	contentHash string
}

// ProgressCallback is called during resolution to report progress.
type ProgressCallback func(event *ProgressEvent)

// ProgressEvent represents a progress update during resolution.
type ProgressEvent struct {
	Type          string // "resolving", "downloading", "resolved", "error"
	ModuleKey     string
	ModuleName    string
	Source        string
	ModulesTotal  int
	ModulesDone   int
	PackagesTotal int
	PackagesDone  int
}

// Resolver orchestrates the BFS module resolution algorithm.
type Resolver struct {
	Progress    ProgressCallback // 16 bytes (data ptr + type ptr)
	fetcher     *Fetcher         // 8 bytes (ptr)
	httpClient  *http.Client     // injected HTTP client for registry API calls (review item 5)
	PackageRoot string           // 16 bytes (data ptr + length)
	Concurrency int              // 8 bytes
	MaxDepth    int              // 0 = unlimited, max depth of module tree
	MaxModules  int              // 0 = unlimited, max number of modules
	VendorDir   string           // Custom vendored directory name (default: "sourcetree")
	RootDir     string           // Root directory for display path computation (set by CLI)
}

// ResolverOption configures a Resolver at construction time (review item 5 /
// ArchAudit R4). The configuration is also settable via the exported fields for
// backwards compatibility; the options are the preferred, freeze-safe entry point.
type ResolverOption func(*Resolver)

// WithFetcher injects a custom Fetcher (e.g. a test double or one configured
// with a custom RoundTripper / getter map).
func WithFetcher(f *Fetcher) ResolverOption {
	return func(r *Resolver) { r.fetcher = f }
}

// WithHTTPClient injects the HTTP client used for registry API queries.
func WithHTTPClient(c *http.Client) ResolverOption {
	return func(r *Resolver) { r.httpClient = c }
}

// WithConcurrency sets the parallel download worker count.
func WithConcurrency(n int) ResolverOption { return func(r *Resolver) { r.Concurrency = n } }

// WithMaxDepth caps the module tree depth (0 = unlimited).
func WithMaxDepth(n int) ResolverOption { return func(r *Resolver) { r.MaxDepth = n } }

// WithMaxModules caps the total module count (0 = unlimited).
func WithMaxModules(n int) ResolverOption { return func(r *Resolver) { r.MaxModules = n } }

// WithResolverVendorDir sets the vendored modules directory name on the Resolver.
func WithResolverVendorDir(d string) ResolverOption { return func(r *Resolver) { r.VendorDir = d } }

// WithPackageRoot sets the package boundary for local-path enforcement.
func WithPackageRoot(p string) ResolverOption { return func(r *Resolver) { r.PackageRoot = p } }

// WithRootDir sets the root directory for display-path computation.
func WithRootDir(d string) ResolverOption { return func(r *Resolver) { r.RootDir = d } }

// WithProgress sets the resolution progress callback.
func WithProgress(p ProgressCallback) ResolverOption { return func(r *Resolver) { r.Progress = p } }

// NewResolver creates a new Resolver with default configuration, applying the
// given options on top of the defaults.
func NewResolver(opts ...ResolverOption) *Resolver {
	r := &Resolver{
		fetcher:     NewFetcher(),
		Concurrency: 4,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// registryClient returns the HTTP client to use for registry API queries: the
// injected client when configured, otherwise nil and doRegistryQuery builds a
// default 30s-timeout client. When the fetcher carries a custom RoundTripper
// and no explicit client was set, mirror it so registry traffic is interceptable.
func (r *Resolver) registryClient() *http.Client {
	if r.httpClient != nil {
		return r.httpClient
	}
	if r.fetcher != nil {
		if rt := r.fetcher.RoundTripper(); rt != nil {
			return &http.Client{Timeout: 30 * time.Second, Transport: rt}
		}
	}
	return nil
}

// report safely calls the progress callback if configured.
func (r *Resolver) report(event *ProgressEvent) {
	if r.Progress != nil {
		r.Progress(event)
	}
}

// displayPath computes a user-friendly display path relative to the root directory.
// Falls back to the base name if the path doesn't start with the root dir.
func (r *Resolver) displayPath(p string) string {
	if r.RootDir == "" {
		return filepath.Base(p)
	}
	rel, err := filepath.Rel(r.RootDir, p)
	if err != nil {
		return filepath.Base(p)
	}
	return rel
}

// downloadPackagesParallel downloads multiple packages concurrently using a worker pool.
func (r *Resolver) downloadPackagesParallel(ctx context.Context, requests []downloadRequest) ([]downloadResult, error) {
	concurrency := r.Concurrency
	if concurrency <= 0 {
		concurrency = 4
	}
	if concurrency > len(requests) {
		concurrency = len(requests)
	}

	results := make([]downloadResult, len(requests))
	fetchErrors := make([]error, len(requests))

	var wg sync.WaitGroup
	semaphore := make(chan struct{}, concurrency)

	for i, req := range requests {
		wg.Add(1)
		go func(idx int, request downloadRequest) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			r.report(&ProgressEvent{
				Type:       "downloading",
				ModuleKey:  request.uniqueID,
				ModuleName: request.packageAddr,
				Source:     request.packageAddr,
			})

			if err := r.fetcher.Fetch(ctx, request.localPath, request.packageAddr); err != nil {
				fetchErrors[idx] = fmt.Errorf("failed to fetch %s: %w", request.packageAddr, err)
				return
			}

			r.report(&ProgressEvent{
				Type:       "downloaded",
				ModuleKey:  request.uniqueID,
				ModuleName: request.packageAddr,
				Source:     request.packageAddr,
			})

			// Compute content hash for content-based deduplication
			contentHash, err := HashModule(request.localPath)
			if err != nil {
				fetchErrors[idx] = fmt.Errorf("failed to hash %s: %w", request.packageAddr, err)
				return
			}

			results[idx] = downloadResult{
				request:     request,
				contentHash: contentHash,
			}
		}(i, req)
	}

	wg.Wait()

	// Check for errors
	for _, err := range fetchErrors {
		if err != nil {
			return nil, err
		}
	}

	return results, nil
}

// Resolve performs BFS module resolution starting from a single root directory
// (the `tofupress module` entry). It discovers the root's module dependencies,
// downloads remote ones, and rewrites remote sources to local vendor paths.
// For multi-entry resolution (the `tofupress tree` case — every .tf-dir under the
// subject is an entry pressed as-is), use ResolveTree.
//
//nolint:gocognit,gocyclo // BFS resolution is inherently complex
func (r *Resolver) Resolve(ctx context.Context, rootDir string) (*ResolvedTree, error) {
	return r.resolve(ctx, rootDir, false)
}

// ResolveTree performs multi-entry BFS module resolution for the `tofupress tree`
// command (ADR-0002): the subject is pressed UNPIVOTED, so every .tf/.tofu-bearing
// directory under the subject is discovered as an independent entry and resolved.
// Local ../ cross-references between entries are preserved verbatim (layout
// unchanged — only remote/registry sources are rewritten and vendored). The
// package boundary for every entry is the subject itself (r.PackageRoot), so a
// local reference escaping the subject is a boundary error.
//
// The synthetic tree.Root has InstallDir = PackageRoot = subject and is never
// scanned for .tf (it is a container, not a module); the discovered entries are
// its children and are the BFS seeds. The bundler stages tree.Root.InstallDir
// (the subject) as-is, so the archive preserves the subject's directory shape.
//
//nolint:gocognit,gocyclo // BFS resolution is inherently complex
func (r *Resolver) ResolveTree(ctx context.Context, subjectDir string) (*ResolvedTree, error) {
	return r.resolve(ctx, subjectDir, true)
}

// resolve is the shared BFS body behind Resolve (single-entry, `module`) and
// ResolveTree (multi-entry, `tree`). multiEntry selects the seed strategy:
// false seeds the single root node (the entry module, scanned for .tf); true
// discovers every .tf/.tofu-bearing directory under rootDir as an entry and
// seeds ALL of them, with the synthetic root as an un-scanned container.
//
//nolint:gocognit,gocyclo // BFS resolution is inherently complex
func (r *Resolver) resolve(ctx context.Context, rootDir string, multiEntry bool) (*ResolvedTree, error) {
	rootPackage := r.PackageRoot

	tree := &ResolvedTree{
		Root: &ModuleNode{
			Key:         "",
			Name:        "root", //nolint:goconst // module name
			InstallDir:  rootDir,
			PackageRoot: rootPackage,
			IsLocal:     true,
		},
		AllModules: []*ModuleNode{},
		Packages:   make(map[string]*DownloadedPackage),
	}

	vendorDirName := r.VendorDir
	if vendorDirName == "" {
		vendorDirName = defaultVendorDir
	}
	sourcetreeDir := filepath.Join(rootDir, vendorDirName)
	tree.VendorDir = vendorDirName
	if err := os.MkdirAll(sourcetreeDir, 0o755); err != nil { //nolint:gosec // G301: 0755 is standard for directories
		return nil, fmt.Errorf("failed to create modules directory: %w", err)
	}

	// Respect context cancellation immediately
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Track downloaded packages for deduplication
	downloadedPackages := make(map[string]string) // packageAddr -> localPath
	contentHashes := make(map[string]string)      // contentHash -> localPath
	visitedPaths := make(map[string]bool)         // Track visited local paths to prevent cycles
	visitedModules := make(map[string]bool)       // Track module keys to detect cycles

	// BFS queue: each item is a module to process
	type queueItem struct {
		node *ModuleNode
		dir  string
	}

	// Track pending downloads and their associated modules
	type downloadInfo struct {
		child     *ModuleNode
		parentDir string
		tfFile    string
		modName   string
	}

	// Seed the BFS queue. `module` (single entry): the root node IS the entry
	// module — scan it for .tf and descend. `tree` (multi entry): the root node is
	// a synthetic container (never scanned); every .tf/.tofu-bearing directory
	// under rootDir is discovered as an entry and seeded, with rootDir as the
	// shared package boundary (ADR-0002). Local ../ refs between entries resolve
	// verbatim because the layout is staged as-is; the BFS dedups a module
	// referenced by several entries via visitedPaths (shared module scanned once).
	var queue []queueItem
	if !multiEntry {
		queue = []queueItem{{node: tree.Root, dir: rootDir}}
		tree.AllModules = append(tree.AllModules, tree.Root)
		visitedPaths[rootDir] = true
	} else {
		entries := discoverSubjectAnchors(rootDir, vendorDirName)
		if len(entries) == 0 {
			return nil, fmt.Errorf("subject %q contains no .tf or .tofu directories; `tofupress tree` presses a tree of modules — use `tofupress module` to press a single module (ADR-0002)", rootDir)
		}
		for _, entryDir := range entries {
			rel, relErr := filepath.Rel(rootDir, entryDir)
			if relErr != nil {
				return nil, fmt.Errorf("failed to compute entry path: %w", relErr)
			}
			rel = filepath.ToSlash(rel)
			name := filepath.Base(entryDir)
			var key string
			if rel == "." {
				name = "root_entry" //nolint:goconst // entry name
				key = "entry.root_entry"
			} else {
				key = "entry." + strings.ReplaceAll(rel, "/", ".")
			}
			entry := &ModuleNode{
				Key:         key,
				Name:        name,
				InstallDir:  entryDir,
				PackageRoot: rootPackage,
				Parent:      tree.Root,
				IsLocal:     true,
			}
			tree.Root.Children = append(tree.Root.Children, entry)
			queue = append(queue, queueItem{node: entry, dir: entryDir})
			tree.AllModules = append(tree.AllModules, entry)
			visitedPaths[entryDir] = true
		}
	}

	// Process modules in BFS order (level by level for parallel downloads)
	for len(queue) > 0 {
		// Respect context cancellation between BFS levels
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		// Process current level
		currentLevel := queue
		queue = nil

		// Collect download requests from this level
		var pendingDownloads []downloadRequest
		downloadInfoMap := make(map[string][]*downloadInfo)

		for _, item := range currentLevel {
			// Check cancellation before scanning each module's directory
			if err := ctx.Err(); err != nil {
				return nil, err
			}

			r.report(&ProgressEvent{
				Type:       "resolving",
				ModuleKey:  item.node.Key,
				ModuleName: item.node.Name,
			})

			// Find all .tf files in this directory
			tfFiles, err := FindTerraformFiles(item.dir)
			if err != nil {
				return nil, fmt.Errorf("failed to find terraform files in %s: %w", r.displayPath(item.dir), err)
			}

			// Extract module blocks from all .tf files
			for _, tfFile := range tfFiles {
				modules, err := ExtractModuleBlocks(tfFile)
				if err != nil {
					return nil, fmt.Errorf("failed to extract module blocks from %s: %w", r.displayPath(tfFile), err)
				}

				// Detect duplicate module names within the same file.
				// Terraform/OpenTofu rejects these, so we should too.
				seenNames := make(map[string]bool, len(modules))
				for _, mod := range modules {
					if seenNames[mod.Name] {
						return nil, fmt.Errorf(
							"duplicate module %q in %s: each module block must have a unique name within the same file",
							mod.Name, r.displayPath(tfFile))
					}
					seenNames[mod.Name] = true
				}

				// Process each module
				for _, mod := range modules {
					// Check for dynamic (variable) source — warn and skip
					if mod.DynamicSource {
						r.report(&ProgressEvent{
							Type:       "warning",
							ModuleKey:  MakeKey(item.node.Key, mod.Name),
							ModuleName: mod.Name,
							Source:     fmt.Sprintf("module %q has a dynamic (variable) source — cannot resolve", mod.Name),
						})
						continue
					}

					// Check for missing source attribute
					if mod.MissingSource {
						return nil, fmt.Errorf("module %q in %s is missing the required 'source' attribute",
							mod.Name, r.displayPath(tfFile))
					}

					// Classify the source
					source := ClassifySource(mod.Source, item.dir)

					// Create child node
					childKey := MakeKey(item.node.Key, mod.Name)
					child := &ModuleNode{
						Key:      childKey,
						Name:     mod.Name,
						Source:   source,
						Parent:   item.node,
						Children: []*ModuleNode{},
					}

					// Handle based on source type
					//nolint:gocritic // if-else chain has substantial logic per branch
					if source.Type == SourceAbsolute {
						return nil, fmt.Errorf("module %s uses absolute module source %q; absolute module source paths are not portable in self-contained artifacts", mod.Name, source.Raw)
					} else if source.Type == SourceLocal {
						// Local module: resolve package root and active module directory
						packageAddr := source.PackageAddr
						if packageAddr == "" {
							packageAddr = source.Raw
						}

						packageRoot := filepath.Clean(filepath.Join(item.dir, packageAddr))
						installDir, err := resolveModuleInstallDir(packageRoot, source.SubDir)
						if err != nil {
							return nil, fmt.Errorf("module %s has invalid subdirectory %q: %w", mod.Name, source.SubDir, err)
						}

						child.InstallDir = installDir
						child.PackageRoot = packageRoot
						child.IsLocal = true
						child.IsRemote = false

						// Cycle detection: check if child's install dir matches any ancestor.
						// MUST run BEFORE boundary check for accurate error messages.
						for ancestor := item.node; ancestor != nil; ancestor = ancestor.Parent {
							if ancestor.InstallDir == installDir {
								return nil, fmt.Errorf("circular dependency detected: module %q references ancestor module %q", child.Key, ancestor.Key)
							}
						}

						// Deduplication: skip if already visited (shared module, not a cycle).
						// MUST run BEFORE boundary check to avoid false boundary errors on shared modules.
						if visitedPaths[installDir] {
							// Add child to parent and tree but don't process again
							item.node.Children = append(item.node.Children, child)
							tree.AllModules = append(tree.AllModules, child)
							continue
						}
						visitedPaths[installDir] = true

						// Check depth limit
						if r.MaxDepth > 0 && child.Depth() > r.MaxDepth {
							return nil, fmt.Errorf("maximum depth %d exceeded at module %s", r.MaxDepth, child.Key)
						}

						// Check module count limit
						if r.MaxModules > 0 && len(tree.AllModules) >= r.MaxModules {
							return nil, fmt.Errorf("module limit %d exceeded at module %s", r.MaxModules, child.Key)
						}

						// Cycle detection by module key
						if visitedModules[child.Key] {
							return nil, fmt.Errorf("circular dependency detected: module %q appears multiple times in the dependency tree", child.Key)
						}
						visitedModules[child.Key] = true

						// Boundary check: ensure module stays within the package root.
						// MUST run AFTER cycle detection to avoid false boundary errors.
						if item.node.PackageRoot != "" {
							boundaryTarget := packageRoot
							if source.SubDir == "" {
								boundaryTarget = installDir
							}
							if err := ensureWithinPackage(item.node.PackageRoot, boundaryTarget); err != nil {
								errMsg := fmt.Sprintf(
									"module %s at %s escapes package boundary: source %s resolves to %s, which is outside package root %s",
									mod.Name, r.displayPath(item.dir), source.Raw, r.displayPath(boundaryTarget), r.displayPath(item.node.PackageRoot))
								if source.Type == SourceLocal {
									errMsg += "\n\nHint: Use // to set the package boundary. " +
										"For example, run from the repository root and use \".//live/myapp\" " +
										"instead of \"./live/myapp\" to include parent directories."
								}
								return nil, fmt.Errorf("%s", errMsg)
							}
							if source.SubDir == "" {
								child.PackageRoot = item.node.PackageRoot
							}
						}

						// Add to queue for processing
						queue = append(queue, queueItem{node: child, dir: installDir})
					} else if source.Type == SourceRegistry {
						// Registry module: query Terraform Registry API to get download URL
						child.IsLocal = false
						child.IsRemote = true

						// Query the registry API to get the download URL
						registryURL, err := queryRegistryAPI(ctx, r.registryClient(), source.RegistryNamespace, source.RegistryName, source.RegistryProvider, mod.Version)
						if err != nil {
							return nil, fmt.Errorf("failed to resolve registry module %s: %w", mod.Name, err)
						}

						// Use the registry URL as the package address
						source.PackageAddr = registryURL
						child.Source = source

						// Check if already downloaded (deduplication)
						localPath, exists := downloadedPackages[registryURL]
						if exists {
							// Already downloaded - resolve subdirectory
							installDir, err := resolveModuleInstallDir(localPath, source.SubDir)
							if err != nil {
								return nil, fmt.Errorf("failed to resolve subdirectory for module %s: %w", mod.Name, err)
							}
							child.PackageRoot = localPath
							child.InstallDir = installDir
						} else {
							// Always track this module as needing the package
							downloadInfoMap[registryURL] = append(downloadInfoMap[registryURL], &downloadInfo{
								child:     child,
								parentDir: item.dir,
								tfFile:    tfFile,
								modName:   mod.Name,
							})

							// But only queue the download once
							if len(downloadInfoMap[registryURL]) == 1 {
								// This is the first module requesting this package
								uniqueID := generateUniqueID(registryURL)
								localPath := filepath.Join(sourcetreeDir, uniqueID)

								pendingDownloads = append(pendingDownloads, downloadRequest{
									packageAddr: registryURL,
									localPath:   localPath,
									uniqueID:    uniqueID,
								})
							}
						}
					} else {
						// Remote module: download if not already downloaded
						child.IsLocal = false
						child.IsRemote = true

						// Check if already downloaded (deduplication)
						localPath, exists := downloadedPackages[source.PackageAddr]
						if exists {
							// Already downloaded - resolve subdirectory
							installDir, err := resolveModuleInstallDir(localPath, source.SubDir)
							if err != nil {
								return nil, fmt.Errorf("failed to resolve subdirectory for module %s: %w", mod.Name, err)
							}
							child.PackageRoot = localPath
							child.InstallDir = installDir
						} else {
							// Always track this module as needing the package
							downloadInfoMap[source.PackageAddr] = append(downloadInfoMap[source.PackageAddr], &downloadInfo{
								child:     child,
								parentDir: item.dir,
								tfFile:    tfFile,
								modName:   mod.Name,
							})

							// But only queue the download once
							if len(downloadInfoMap[source.PackageAddr]) == 1 {
								// This is the first module requesting this package
								uniqueID := generateUniqueID(source.PackageAddr)
								localPath := filepath.Join(sourcetreeDir, uniqueID)

								pendingDownloads = append(pendingDownloads, downloadRequest{
									packageAddr: source.PackageAddr,
									localPath:   localPath,
									uniqueID:    uniqueID,
								})
							}
						}
					}

					// Add child to parent and tree
					item.node.Children = append(item.node.Children, child)
					tree.AllModules = append(tree.AllModules, child)
				}
			}
		}

		// Download all pending remote modules in parallel
		if len(pendingDownloads) > 0 {
			results, err := r.downloadPackagesParallel(ctx, pendingDownloads)
			if err != nil {
				return nil, err
			}

			// Process download results, collecting limit violations
			var errs []error
			for _, result := range results {
				infos := downloadInfoMap[result.request.packageAddr]
				if len(infos) == 0 {
					continue
				}

				localPath := result.request.localPath

				// Record the download
				downloadedPackages[result.request.packageAddr] = localPath

				// Check for content-based deduplication
				existingPath, contentExists := contentHashes[result.contentHash]
				if contentExists {
					// Duplicate content - remove the download and reuse existing path
					if removeErr := os.RemoveAll(localPath); removeErr != nil {
						r.report(&ProgressEvent{
							Type:       "warning",
							ModuleKey:  result.request.uniqueID,
							ModuleName: result.request.packageAddr,
							Source: fmt.Sprintf(
								"failed to remove duplicate download %s: %v",
								localPath, removeErr,
							),
						})
					}
					localPath = existingPath
					downloadedPackages[result.request.packageAddr] = localPath
				} else {
					// New content - record it
					contentHashes[result.contentHash] = localPath
					tree.Packages[result.request.uniqueID] = &DownloadedPackage{
						PackageAddr: result.request.packageAddr,
						LocalDir:    localPath,
						ContentHash: result.contentHash,
					}
				}

				// Set install directory and rewrite source for all modules using this package
				for _, info := range infos {
					installDir, err := resolveModuleInstallDir(localPath, info.child.Source.SubDir)
					if err != nil {
						return nil, fmt.Errorf("failed to resolve subdirectory for module %s: %w", info.modName, err)
					}

					info.child.PackageRoot = localPath
					info.child.InstallDir = installDir

					relPath, err := filepath.Rel(info.parentDir, localPath)
					if err != nil {
						return nil, fmt.Errorf("failed to compute relative path: %w", err)
					}
					newSource := moduleSourcePath(relPath, info.child.Source.SubDir)

					// Registry-source modules carry `version = "x.y.z"`, which OpenTofu/Terraform
					// reject on a rewritten local source (`Invalid registry module source address`,
					// review item 1). Drop the `version` attribute when rewriting to a local path.
					// All other source types never carry `version`, so they keep the plain rewrite.
					rewriteErr := RewriteModuleSource(info.tfFile, info.modName, newSource)
					if info.child.Source.Type == SourceRegistry {
						rewriteErr = RewriteModuleSourceAndDropVersion(info.tfFile, info.modName, newSource)
					}
					if rewriteErr != nil {
						return nil, fmt.Errorf("failed to rewrite source for module %s: %w", info.modName, rewriteErr)
					}

					// Check depth limit
					if r.MaxDepth > 0 && info.child.Depth() > r.MaxDepth {
						errs = append(errs, fmt.Errorf("maximum depth %d exceeded at module %s", r.MaxDepth, info.child.Key))
						continue
					}

					// Check module count limit
					if r.MaxModules > 0 && len(tree.AllModules) >= r.MaxModules {
						errs = append(errs, fmt.Errorf("module limit %d exceeded at module %s", r.MaxModules, info.child.Key))
						continue
					}

					// Cycle detection by module key
					if visitedModules[info.child.Key] {
						return nil, fmt.Errorf("cycle detected: module %q appears multiple times in the dependency tree", info.child.Key)
					}
					visitedModules[info.child.Key] = true

					// Add to queue for processing
					queue = append(queue, queueItem{node: info.child, dir: installDir})
				}
			}

			// Report all limit violations from this batch
			if len(errs) > 0 {
				return nil, fmt.Errorf("resolution limits exceeded: %w", errors.Join(errs...))
			}
		}
	}

	return tree, nil
}

// discoverSubjectAnchors walks the subject directory recursively and returns the
// sorted absolute paths of every directory that directly contains at least one
// .tf/.tofu file (an anchor per ADR-0001 / an entry per ADR-0002). It skips VCS
// and generated dirs (.git, .terraform) AND the vendor directory name, because
// vendored content is never an anchor (ADR-0001) — pre-resolution there are no
// tree.Packages yet, so the vendor carveout is by name here, not by collected
// package roots. Hidden dirs are skipped to avoid descending into .git etc.;
// the vendor dir itself is hidden by convention (_vendor) but is skipped by
// exact name match in case a user picks a non-hidden --vendor-dir.
func discoverSubjectAnchors(subjectDir, vendorDirName string) []string {
	subject := filepath.Clean(subjectDir)
	var anchors []string
	var walk func(dir string)
	walk = func(dir string) {
		if isGeneratedOrVCSPath(dir) {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return // tolerate vanished paths during discovery
		}
		subdirs, hasTF := classifySubjectDirEntries(entries, dir, vendorDirName)
		if hasTF {
			anchors = append(anchors, dir)
		}
		for _, sub := range subdirs {
			walk(sub)
		}
	}
	walk(subject)
	sort.Strings(anchors)
	return anchors
}

// classifySubjectDirEntries splits a directory's entries into the subdirectory
// paths to recurse into (skipping the vendor dir by name and hidden dirs) and
// reports whether the directory directly contains at least one .tf/.tofu file.
// Single os.ReadDir scan; mirrors strip.go's classifyAnchorEntries but skips
// the vendor dir by name (pre-resolution there are no tree.Packages yet).
func classifySubjectDirEntries(entries []os.DirEntry, dir, vendorDirName string) (subdirs []string, hasTF bool) {
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			// Skip the vendor dir by name (vendored content is never an anchor).
			if name == vendorDirName {
				continue
			}
			// Skip hidden directories (.git, .terraform, .github, ...).
			if strings.HasPrefix(name, ".") {
				continue
			}
			subdirs = append(subdirs, filepath.Join(dir, name))
			continue
		}
		if isTerraformConfigFile(name) {
			hasTF = true
		}
	}
	return subdirs, hasTF
}

// generateUniqueID creates a unique identifier for a package address.
// Uses SHA-256 truncated to 16 hex characters (64 bits) to avoid collision risk.
func generateUniqueID(packageAddr string) string {
	hash := sha256.Sum256([]byte(packageAddr))
	return hex.EncodeToString(hash[:8]) // First 8 bytes = 16 hex chars = 64 bits
}

// resolveModuleInstallDir computes the active module directory given a package root
// and an optional subdirectory (from a //subdir source reference).
func resolveModuleInstallDir(packageRoot, subDir string) (string, error) {
	if subDir == "" || subDir == "." {
		return packageRoot, nil
	}

	installDir := filepath.Clean(filepath.Join(packageRoot, subDir))
	if err := ensureWithinPackage(packageRoot, installDir); err != nil {
		return "", err
	}
	return installDir, nil
}

// ensureWithinPackage verifies that target is within the package root boundary.
func ensureWithinPackage(packageRoot, target string) error {
	if packageRoot == "" {
		return nil
	}

	relPath, err := filepath.Rel(packageRoot, target)
	if err != nil {
		return fmt.Errorf("failed to check package boundary: %w", err)
	}
	if relPath == ".." || strings.HasPrefix(relPath, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path %s escapes package root %s", target, packageRoot)
	}
	return nil
}

// moduleSourcePath builds a local source string from a package-relative path and an
// optional subdirectory, preserving //subdir semantics in the rewritten source.
func moduleSourcePath(packageRelPath, subDir string) string {
	localPath := filepath.ToSlash(packageRelPath)
	if !strings.HasPrefix(localPath, "./") && !strings.HasPrefix(localPath, "../") {
		localPath = "./" + localPath
	}
	if subDir == "" || subDir == "." {
		return localPath
	}
	return localPath + "//" + path.Clean(filepath.ToSlash(subDir))
}

// registryResponse represents the Terraform Registry API response.
type registryResponse struct {
	Source  string `json:"source"`
	Version string `json:"version"`
	Tag     string `json:"tag"`
}

// maxRegistryResponseSize is the maximum size of a registry API response (1MB).
const maxRegistryResponseSize = 1 << 20 // 1MB

// testRegistryBaseURL is set by tests to point to a local httptest server.
// It defaults to the production Terraform Registry URL.
var testRegistryBaseURL string

// queryRegistryAPI queries the Terraform Registry API to get the download URL for a module.
// It retries transient errors (5xx status codes, network failures) with exponential backoff.
// testRegistryBaseURL takes precedence (set by in-process tests); the TOFUPRESS_REGISTRY_BASE_URL
// env var allows driving the CLI binary at a local httptest stub without touching the network.
// The client parameter may be nil, in which case a default 30s-timeout client is used; the
// Resolver injects its configured client here (review item 5).
func queryRegistryAPI(ctx context.Context, client *http.Client, namespace, name, provider, version string) (string, error) {
	baseURL := testRegistryBaseURL
	if baseURL == "" {
		if env := os.Getenv("TOFUPRESS_REGISTRY_BASE_URL"); env != "" {
			baseURL = env
		} else {
			baseURL = "https://registry.terraform.io"
		}
	}

	apiURL := fmt.Sprintf("%s/v1/modules/%s/%s/%s",
		baseURL, url.PathEscape(namespace), url.PathEscape(name), url.PathEscape(provider))

	if version != "" {
		apiURL = fmt.Sprintf("%s/%s", apiURL, url.PathEscape(version))
	}

	const maxRetries = 3
	var lastErr error

	for attempt := range maxRetries {
		if attempt > 0 {
			// Exponential backoff: 1s, 2s, 4s
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(backoff):
			}
		}

		result, err := doRegistryQuery(ctx, client, apiURL)
		if err == nil {
			return result, nil
		}

		// Only retry on transient errors (5xx status codes, network errors)
		if !isTransientError(err) {
			return "", err
		}
		lastErr = err
	}

	return "", fmt.Errorf("registry API query failed after %d attempts for %s/%s/%s: %w",
		maxRetries, namespace, name, provider, lastErr)
}

// doRegistryQuery executes a single HTTP query to the registry API.
func doRegistryQuery(ctx context.Context, client *http.Client, apiURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, http.NoBody) //nolint:gosec // G704: apiURL is built from registry-namespace/name/provider path-escaped inputs, not user-controlled freeform URL
	if err != nil {
		return "", fmt.Errorf("failed to create registry API request: %w", err)
	}
	req.Header.Set("User-Agent", "tofupress/dev")

	if client == nil {
		client = &http.Client{
			Timeout: 30 * time.Second,
		}
	}

	resp, err := client.Do(req) //nolint:gosec // G704: apiURL is built from trusted registry API inputs, not user-controlled freeform URL
	if err != nil {
		return "", fmt.Errorf("failed to query registry API: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // best effort

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", &registryAPIError{
			StatusCode: resp.StatusCode,
			URL:        apiURL,
			Body:       string(body),
		}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRegistryResponseSize))
	if err != nil {
		return "", fmt.Errorf("failed to read registry API response: %w", err)
	}

	var registryResp registryResponse
	if err := json.Unmarshal(body, &registryResp); err != nil {
		return "", fmt.Errorf("failed to parse registry API response: %w", err)
	}

	if registryResp.Source == "" {
		return "", fmt.Errorf("registry API did not return a source URL")
	}

	gitURL := fmt.Sprintf("git::%s", registryResp.Source)
	if registryResp.Tag != "" {
		gitURL = fmt.Sprintf("%s?ref=%s", gitURL, registryResp.Tag)
	}

	return gitURL, nil
}

// registryAPIError is a typed error for non-200 registry API responses.
type registryAPIError struct {
	StatusCode int
	URL        string
	Body       string
}

func (e *registryAPIError) Error() string {
	return fmt.Sprintf("registry API returned status %d for %s: %s", e.StatusCode, e.URL, e.Body)
}

// isTransientError returns true for errors that should be retried.
// Only 5xx status codes and network errors are considered transient.
func isTransientError(err error) bool {
	var apiErr *registryAPIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode >= 500
	}
	// Only network/transport errors are transient.
	// JSON parse errors, missing source errors, etc. are not retried.
	return isNetworkError(err)
}

// isNetworkError returns true if the error is a network/transport-level error
// (e.g., DNS failure, connection refused, TLS handshake failure, timeout).
func isNetworkError(err error) bool {
	// Check for common Go net error types
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	// Check for url.Error which wraps network/transport errors
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	// ErrBodyNotAllowed is not a network error
	return false
}
