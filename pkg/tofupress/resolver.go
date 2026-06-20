package tofupress

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Resolver orchestrates the BFS module resolution algorithm.
type Resolver struct {
	fetcher *Fetcher
}

// NewResolver creates a new Resolver with default configuration.
func NewResolver() *Resolver {
	return &Resolver{
		fetcher: NewFetcher(),
	}
}

// Resolve performs BFS module resolution starting from the root directory.
// It discovers all modules, downloads remote ones, and rewrites sources to local paths.
//
//nolint:gocognit,gocyclo // BFS resolution is inherently complex
func (r *Resolver) Resolve(ctx context.Context, rootDir string) (*ResolvedTree, error) {
	tree := &ResolvedTree{
		Root: &ModuleNode{
			Key:        "",
			Name:       "root", //nolint:goconst // module name
			InstallDir: rootDir,
			IsLocal:    true,
		},
		AllModules: []*ModuleNode{},
		Packages:   make(map[string]*DownloadedPackage),
	}

	sourcetreeDir := filepath.Join(rootDir, "sourcetree")
	if err := os.MkdirAll(sourcetreeDir, 0o755); err != nil { //nolint:gosec // G301: 0755 is standard for directories
		return nil, fmt.Errorf("failed to create sourcetree directory: %w", err)
	}

	// Track downloaded packages for deduplication
	downloadedPackages := make(map[string]string) // packageAddr -> localPath
	contentHashes := make(map[string]string)      // contentHash -> localPath
	visitedPaths := make(map[string]bool)         // Track visited local paths to prevent cycles

	// BFS queue: each item is a module to process
	type queueItem struct {
		node *ModuleNode
		dir  string
	}

	queue := []queueItem{{node: tree.Root, dir: rootDir}}
	tree.AllModules = append(tree.AllModules, tree.Root)
	visitedPaths[rootDir] = true

	// Process modules in BFS order
	for len(queue) > 0 {
		// Dequeue
		item := queue[0]
		queue = queue[1:]

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
				if source.Type == SourceLocal {
					// Local module: resolve path relative to parent
					localPath := filepath.Join(item.dir, source.Raw)
					child.InstallDir = localPath
					child.IsLocal = true
					child.IsRemote = false

					// Cycle detection: skip if already visited
					if visitedPaths[localPath] {
						// Add child to parent and tree but don't process again
						item.node.Children = append(item.node.Children, child)
						tree.AllModules = append(tree.AllModules, child)
						continue
					}
					visitedPaths[localPath] = true

					// Add to queue for processing
					queue = append(queue, queueItem{node: child, dir: localPath})
				} else if source.Type == SourceRegistry {
					// Registry module: query Terraform Registry API to get download URL
					child.IsLocal = false
					child.IsRemote = true

					// Query the registry API to get the download URL
					registryURL, err := queryRegistryAPI(source.RegistryNamespace, source.RegistryName, source.RegistryProvider, mod.Version)
					if err != nil {
						return nil, fmt.Errorf("failed to resolve registry module %s: %w", mod.Name, err)
					}

					// Use the registry URL as the package address
					source.PackageAddr = registryURL
					child.Source = source

					// Check if already downloaded (deduplication)
					localPath, exists := downloadedPackages[registryURL]
					if !exists {
						// Download the package
						uniqueID := generateUniqueID(registryURL)
						localPath = filepath.Join(sourcetreeDir, uniqueID)

						if err := r.fetcher.Fetch(ctx, localPath, registryURL); err != nil {
							return nil, fmt.Errorf("failed to fetch registry module %s from %s: %w", mod.Name, registryURL, err)
						}

						// Record the download
						downloadedPackages[registryURL] = localPath

						// Compute content hash for content-based deduplication
						contentHash, hashErr := HashModule(localPath)
						if hashErr != nil {
							return nil, fmt.Errorf("failed to compute content hash for module %s: %w", mod.Name, hashErr)
						}

						// Check for content-based deduplication
						existingPath, contentExists := contentHashes[contentHash]
						if contentExists {
							// Duplicate content - remove the download and reuse existing path
							os.RemoveAll(localPath) //nolint:errcheck // best-effort cleanup
							localPath = existingPath
							downloadedPackages[registryURL] = localPath
						} else {
							// New content - record it
							contentHashes[contentHash] = localPath
							tree.Packages[uniqueID] = &DownloadedPackage{
								PackageAddr: registryURL,
								LocalDir:    localPath,
								ContentHash: contentHash,
							}
						}

						// Set install directory
						child.InstallDir = localPath

						// Rewrite the source in the .tf file to point to local path
						relPath, err := filepath.Rel(item.dir, localPath)
						if err != nil {
							return nil, fmt.Errorf("failed to compute relative path: %w", err)
						}
						newSource := "./" + relPath

						if err := RewriteModuleSource(tfFile, mod.Name, newSource); err != nil {
							return nil, fmt.Errorf("failed to rewrite source for module %s: %w", mod.Name, err)
						}

						// Add to queue for processing (only for new downloads)
						queue = append(queue, queueItem{node: child, dir: localPath})
					} else {
						// Already downloaded - just set install directory, don't reprocess
						child.InstallDir = localPath
					}
				} else {
					// Remote module: download if not already downloaded
					child.IsLocal = false
					child.IsRemote = true

					// Check if already downloaded (deduplication)
					localPath, exists := downloadedPackages[source.PackageAddr]
					if !exists {
						// Download the package
						uniqueID := generateUniqueID(source.PackageAddr)
						localPath = filepath.Join(sourcetreeDir, uniqueID)

						if err := r.fetcher.Fetch(ctx, localPath, source.PackageAddr); err != nil {
							return nil, fmt.Errorf("failed to fetch module %s from %s: %w", mod.Name, source.PackageAddr, err)
						}

						// Record the download
						downloadedPackages[source.PackageAddr] = localPath

						// Compute content hash for content-based deduplication
						contentHash, hashErr := HashModule(localPath)
						if hashErr != nil {
							return nil, fmt.Errorf("failed to compute content hash for module %s: %w", mod.Name, hashErr)
						}

						// Check for content-based deduplication
						existingPath, contentExists := contentHashes[contentHash]
						if contentExists {
							// Duplicate content - remove the download and reuse existing path
							os.RemoveAll(localPath) //nolint:errcheck // best-effort cleanup
							localPath = existingPath
							downloadedPackages[source.PackageAddr] = localPath
						} else {
							// New content - record it
							contentHashes[contentHash] = localPath
							tree.Packages[uniqueID] = &DownloadedPackage{
								PackageAddr: source.PackageAddr,
								LocalDir:    localPath,
								ContentHash: contentHash,
							}
						}

						// Set install directory
						child.InstallDir = localPath

						// Rewrite the source in the .tf file to point to local path
						relPath, err := filepath.Rel(item.dir, localPath)
						if err != nil {
							return nil, fmt.Errorf("failed to compute relative path: %w", err)
						}
						newSource := "./" + relPath

						if err := RewriteModuleSource(tfFile, mod.Name, newSource); err != nil {
							return nil, fmt.Errorf("failed to rewrite source for module %s: %w", mod.Name, err)
						}

						// Add to queue for processing (only for new downloads)
						queue = append(queue, queueItem{node: child, dir: localPath})
					} else {
						// Already downloaded - just set install directory, don't reprocess
						child.InstallDir = localPath
					}
				}

				// Add child to parent and tree
				item.node.Children = append(item.node.Children, child)
				tree.AllModules = append(tree.AllModules, child)
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

// registryResponse represents the Terraform Registry API response.
type registryResponse struct {
	Source  string `json:"source"`
	Version string `json:"version"`
	Tag     string `json:"tag"`
}

// queryRegistryAPI queries the Terraform Registry API to get the download URL for a module.
func queryRegistryAPI(namespace, name, provider, version string) (string, error) {
	// Construct the registry API URL
	// Format: https://registry.terraform.io/v1/modules/{namespace}/{name}/{provider}
	apiURL := fmt.Sprintf("https://registry.terraform.io/v1/modules/%s/%s/%s",
		namespace, name, provider)

	// Add version if specified
	if version != "" {
		apiURL = fmt.Sprintf("%s/%s", apiURL, version)
	}

	// Create HTTP client with timeout
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	// Make the request
	resp, err := client.Get(apiURL) //nolint:gosec // G107: URL is constructed from validated parts
	if err != nil {
		return "", fmt.Errorf("failed to query registry API: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // best effort

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry API returned status %d for %s", resp.StatusCode, apiURL)
	}

	// Read the response body
	body, err := io.ReadAll(resp.Body)
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
