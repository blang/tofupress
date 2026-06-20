package tofupress

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
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

	// BFS queue: each item is a module to process
	type queueItem struct {
		node *ModuleNode
		dir  string
	}

	queue := []queueItem{{node: tree.Root, dir: rootDir}}
	tree.AllModules = append(tree.AllModules, tree.Root)

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

					// Add to queue for processing
					queue = append(queue, queueItem{node: child, dir: localPath})
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

					// Add to queue for processing (remote modules may have their own dependencies)
					queue = append(queue, queueItem{node: child, dir: localPath})
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
