package tofupress

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
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
	PackageRoot string           // 16 bytes (data ptr + length)
	Concurrency int              // 8 bytes
	MaxDepth    int              // 0 = unlimited, max depth of module tree
	MaxModules  int              // 0 = unlimited, max number of modules
	VendorDir   string           // Custom vendored directory name (default: "sourcetree")
}

// NewResolver creates a new Resolver with default configuration.
func NewResolver() *Resolver {
	return &Resolver{
		fetcher:     NewFetcher(),
		Concurrency: 4,
	}
}

// report safely calls the progress callback if configured.
func (r *Resolver) report(event *ProgressEvent) {
	if r.Progress != nil {
		r.Progress(event)
	}
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
	errors := make([]error, len(requests))

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
				errors[idx] = fmt.Errorf("failed to fetch %s: %w", request.packageAddr, err)
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
				errors[idx] = fmt.Errorf("failed to hash %s: %w", request.packageAddr, err)
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
	for _, err := range errors {
		if err != nil {
			return nil, err
		}
	}

	return results, nil
}

// Resolve performs BFS module resolution starting from the root directory.
// It discovers all modules, downloads remote ones, and rewrites sources to local paths.
//
//nolint:gocognit,gocyclo // BFS resolution is inherently complex
func (r *Resolver) Resolve(ctx context.Context, rootDir string) (*ResolvedTree, error) {
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
		vendorDirName = "sourcetree"
	}
	sourcetreeDir := filepath.Join(rootDir, vendorDirName)
	tree.VendorDir = vendorDirName
	if err := os.MkdirAll(sourcetreeDir, 0o755); err != nil { //nolint:gosec // G301: 0755 is standard for directories
		return nil, fmt.Errorf("failed to create sourcetree directory: %w", err)
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

	queue := []queueItem{{node: tree.Root, dir: rootDir}}
	tree.AllModules = append(tree.AllModules, tree.Root)
	visitedPaths[rootDir] = true

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
				return nil, fmt.Errorf("failed to find terraform files in %s: %w", item.dir, err)
			}

			// Extract module blocks from all .tf files
			for _, tfFile := range tfFiles {
				modules, err := ExtractModuleBlocks(tfFile)
				if err != nil {
					return nil, fmt.Errorf("failed to extract module blocks from %s: %w", tfFile, err)
				}

				// Process each module
				for _, mod := range modules {
					// Check for missing source attribute
					if mod.MissingSource {
						return nil, fmt.Errorf("module %q in %s is missing the required 'source' attribute",
							mod.Name, tfFile)
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

						if item.node.PackageRoot != "" {
							boundaryTarget := packageRoot
							if source.SubDir == "" {
								boundaryTarget = installDir
							}
							if err := ensureWithinPackage(item.node.PackageRoot, boundaryTarget); err != nil {
								return nil, fmt.Errorf("module %s at %s escapes package boundary: source %s resolves to %s, which is outside package root %s",
									mod.Name, item.dir, source.Raw, boundaryTarget, item.node.PackageRoot)
							}
							if source.SubDir == "" {
								packageRoot = item.node.PackageRoot
							}
						}

						child.InstallDir = installDir
						child.PackageRoot = packageRoot
						child.IsLocal = true
						child.IsRemote = false

						// Cycle detection: check if child's install dir matches any ancestor
						for ancestor := item.node; ancestor != nil; ancestor = ancestor.Parent {
							if ancestor.InstallDir == installDir {
								return nil, fmt.Errorf("cycle detected: module %q references ancestor module %q", child.Key, ancestor.Key)
							}
						}

						// Deduplication: skip if already visited (shared module, not a cycle)
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
							return nil, fmt.Errorf("cycle detected: module %q appears multiple times in the dependency tree", child.Key)
						}
						visitedModules[child.Key] = true

						// Add to queue for processing
						queue = append(queue, queueItem{node: child, dir: installDir})
					} else if source.Type == SourceRegistry {
						// Registry module: query Terraform Registry API to get download URL
						child.IsLocal = false
						child.IsRemote = true

						// Query the registry API to get the download URL
						registryURL, err := queryRegistryAPI(ctx, source.RegistryNamespace, source.RegistryName, source.RegistryProvider, mod.Version)
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

			// Process download results
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
					os.RemoveAll(localPath) //nolint:errcheck,gosec // best-effort cleanup
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

					if err := RewriteModuleSource(info.tfFile, info.modName, newSource); err != nil {
						return nil, fmt.Errorf("failed to rewrite source for module %s: %w", info.modName, err)
					}

					// Check depth limit
					if r.MaxDepth > 0 && info.child.Depth() > r.MaxDepth {
						return nil, fmt.Errorf("maximum depth %d exceeded at module %s", r.MaxDepth, info.child.Key)
					}

					// Check module count limit
					if r.MaxModules > 0 && len(tree.AllModules) >= r.MaxModules {
						return nil, fmt.Errorf("module limit %d exceeded at module %s", r.MaxModules, info.child.Key)
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
		}
	}

	return tree, nil
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

// queryRegistryAPI queries the Terraform Registry API to get the download URL for a module.
func queryRegistryAPI(ctx context.Context, namespace, name, provider, version string) (string, error) {
	// Construct the registry API URL with proper URL encoding
	// Format: https://registry.terraform.io/v1/modules/{namespace}/{name}/{provider}
	apiURL := fmt.Sprintf("https://registry.terraform.io/v1/modules/%s/%s/%s",
		url.PathEscape(namespace), url.PathEscape(name), url.PathEscape(provider))

	// Add version if specified
	if version != "" {
		apiURL = fmt.Sprintf("%s/%s", apiURL, url.PathEscape(version))
	}

	// Create HTTP request with context for cancellation support
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("failed to create registry API request: %w", err)
	}

	// Create HTTP client with timeout
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	// Make the request
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to query registry API: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // best effort

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry API returned status %d for %s", resp.StatusCode, apiURL)
	}

	// Read the response body with size limit to prevent OOM
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRegistryResponseSize))
	if err != nil {
		return "", fmt.Errorf("failed to read registry API response: %w", err)
	}

	// Parse the JSON response
	var registryResp registryResponse
	if err := json.Unmarshal(body, &registryResp); err != nil {
		return "", fmt.Errorf("failed to parse registry API response: %w", err)
	}

	if registryResp.Source == "" {
		return "", fmt.Errorf("registry API did not return a source URL")
	}

	// Construct a git source URL from the source and tag
	// Format: git::https://github.com/...?ref=vX.Y.Z
	gitURL := fmt.Sprintf("git::%s", registryResp.Source)
	if registryResp.Tag != "" {
		gitURL = fmt.Sprintf("%s?ref=%s", gitURL, registryResp.Tag)
	}

	return gitURL, nil
}
