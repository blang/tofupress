// Package tofupress provides module resolution and bundling for Terraform/OpenTofu.
package tofupress

import "strings"

// SourceType classifies how a module source should be resolved.
type SourceType int

// Source type constants identify the protocol or mechanism used to fetch a module.
const (
	SourceUnknown  SourceType = iota // Unrecognized or malformed source
	SourceLocal                      // ./modules/foo, ../shared
	SourceAbsolute                   // /absolute/path/to/module, rejected by default
	SourceGit                        // git::https://...
	SourceHTTP                       // https://...archive.tar.gz
	SourceRegistry                   // hashicorp/consul/aws
	SourceS3                         // s3::https://...
	SourceGCS                        // gcs::https://...
	SourceOCI                        // oci://registry/repo?tag=v1
)

// ModuleSource represents a parsed module source attribute.
type ModuleSource struct {
	Raw               string
	PackageAddr       string
	SubDir            string
	Ref               string
	RegistryNamespace string // For registry modules: namespace
	RegistryName      string // For registry modules: module name
	RegistryProvider  string // For registry modules: provider
	Type              SourceType
}

// String constants for display and comparison
const (
	SourceDisplayLocal    = "local"
	SourceDisplayRegistry = "registry"
	SourceDisplayRoot     = "root"
	hclBlockTypeModule    = "module"
)

// ModuleNode represents a single module instance in the resolved tree.
// The same package can appear multiple times in the tree with different keys.
type ModuleNode struct {
	Parent      *ModuleNode
	Source      ModuleSource
	Key         string
	Name        string
	InstallDir  string
	PackageRoot string
	Children    []*ModuleNode
	IsLocal     bool
	IsRemote    bool
}

// DownloadedPackage tracks a unique remote package that was downloaded.
type DownloadedPackage struct {
	PackageAddr          string   // Normalized package address from the first/canonical source
	LocalDir             string   // Final canonical sourcetree directory after identity planning
	ContentHash          string   // SHA-256 hash observed immediately after download/fetch/copy
	SourcetreeID         string   // Stable content-addressed sourcetree directory ID
	FinalHash            string   // SHA-256 hash of final archive-visible package content
	CanonicalPackageAddr string   // Source address chosen as canonical representative for this package
	PackageAddrs         []string // All source addresses represented by this final package
	ModuleKeys           []string // All module keys using this final package
	Deduplicated         bool     // True when more than one source/module maps to this package
}

// ResolvedTree is the complete resolved module dependency tree.
type ResolvedTree struct {
	Root       *ModuleNode
	Packages   map[string]*DownloadedPackage
	VendorDir  string
	AllModules []*ModuleNode
}

// MakeKey creates a dot-separated key from parent key and module name.
func MakeKey(parentKey, moduleName string) string {
	if parentKey == "" {
		return moduleName
	}
	return parentKey + "." + moduleName
}

// FindByKey searches the module tree for a node with the given key.
func (n *ModuleNode) FindByKey(key string) *ModuleNode {
	if n.Key == key {
		return n
	}

	for _, child := range n.Children {
		if found := child.FindByKey(key); found != nil {
			return found
		}
	}

	return nil
}

// String returns a human-readable representation of the source type.
func (s SourceType) String() string {
	switch s {
	case SourceUnknown:
		return "unknown"
	case SourceLocal:
		return SourceDisplayLocal
	case SourceAbsolute:
		return "absolute"
	case SourceGit:
		return "git"
	case SourceHTTP:
		return "http"
	case SourceRegistry:
		return SourceDisplayRegistry
	case SourceS3:
		return "s3"
	case SourceGCS:
		return "gcs"
	case SourceOCI:
		return "oci"
	default:
		return "unknown"
	}
}

// IsRemote returns true if the source type requires downloading.
func (s SourceType) IsRemote() bool {
	switch s {
	case SourceGit, SourceHTTP, SourceRegistry, SourceS3, SourceGCS, SourceOCI:
		return true
	default:
		return false
	}
}

// String returns a human-readable representation of the module source.
func (m ModuleSource) String() string {
	if m.PackageAddr != "" {
		return RedactSourceAddress(m.PackageAddr)
	}
	return RedactSourceAddress(m.Raw)
}

// Depth returns the depth of this node in the tree (root = 0).
func (n *ModuleNode) Depth() int {
	if n.Parent == nil {
		return 0
	}
	return n.Parent.Depth() + 1
}

// Path returns the full path from root to this node as a slice of names.
func (n *ModuleNode) Path() []string {
	if n.Parent == nil {
		return []string{n.Name}
	}
	return append(n.Parent.Path(), n.Name)
}

// String returns a human-readable representation of the module node.
func (n *ModuleNode) String() string {
	if n == nil {
		return "<nil>"
	}
	return n.Key
}

// HasChildren returns true if this module has child modules.
func (n *ModuleNode) HasChildren() bool {
	return len(n.Children) > 0
}

// ChildCount returns the number of direct child modules.
func (n *ModuleNode) ChildCount() int {
	return len(n.Children)
}

// TotalDescendants returns the total number of descendants (children + grandchildren + ...).
func (n *ModuleNode) TotalDescendants() int {
	count := len(n.Children)
	for _, child := range n.Children {
		count += child.TotalDescendants()
	}
	return count
}

// IsRoot returns true if this is the root module.
func (n *ModuleNode) IsRoot() bool {
	return n.Parent == nil
}

// Root returns the root module of the tree.
func (n *ModuleNode) Root() *ModuleNode {
	if n.Parent == nil {
		return n
	}
	return n.Parent.Root()
}

// Siblings returns all modules that share the same parent (excluding self).
func (n *ModuleNode) Siblings() []*ModuleNode {
	if n.Parent == nil {
		return nil
	}

	var siblings []*ModuleNode
	for _, sibling := range n.Parent.Children {
		if sibling != n {
			siblings = append(siblings, sibling)
		}
	}
	return siblings
}

// Ancestors returns all ancestor modules from parent to root.
func (n *ModuleNode) Ancestors() []*ModuleNode {
	if n.Parent == nil {
		return nil
	}
	return append(n.Parent.Ancestors(), n.Parent)
}

// Walk traverses the tree depth-first, calling fn for each node.
func (n *ModuleNode) Walk(fn func(*ModuleNode)) {
	fn(n)
	for _, child := range n.Children {
		child.Walk(fn)
	}
}

// FindByName searches for a child module with the given name (direct children only).
func (n *ModuleNode) FindByName(name string) *ModuleNode {
	for _, child := range n.Children {
		if child.Name == name {
			return child
		}
	}
	return nil
}

// FindAllByName searches the entire subtree for modules with the given name.
func (n *ModuleNode) FindAllByName(name string) []*ModuleNode {
	var results []*ModuleNode
	if n.Name == name {
		results = append(results, n)
	}
	for _, child := range n.Children {
		results = append(results, child.FindAllByName(name)...)
	}
	return results
}

// Contains returns true if the tree contains a module with the given key.
func (t *ResolvedTree) Contains(key string) bool {
	for _, mod := range t.AllModules {
		if mod.Key == key {
			return true
		}
	}
	return false
}

// Find returns the module with the given key, or nil if not found.
func (t *ResolvedTree) Find(key string) *ModuleNode {
	for _, mod := range t.AllModules {
		if mod.Key == key {
			return mod
		}
	}
	return nil
}

// ModuleCount returns the total number of modules in the tree.
func (t *ResolvedTree) ModuleCount() int {
	return len(t.AllModules)
}

// PackageCount returns the number of unique downloaded packages.
func (t *ResolvedTree) PackageCount() int {
	return len(t.Packages)
}

// LocalModuleCount returns the number of local modules.
func (t *ResolvedTree) LocalModuleCount() int {
	count := 0
	for _, mod := range t.AllModules {
		if mod.IsLocal {
			count++
		}
	}
	return count
}

// RemoteModuleCount returns the number of remote modules.
func (t *ResolvedTree) RemoteModuleCount() int {
	count := 0
	for _, mod := range t.AllModules {
		if mod.IsRemote {
			count++
		}
	}
	return count
}

// PrintTree returns a string representation of the module tree.
func (t *ResolvedTree) PrintTree() string {
	if t.Root == nil {
		return "<empty tree>"
	}

	var lines []string
	t.printNode(t.Root, "", &lines)
	return strings.Join(lines, "\n")
}

func (t *ResolvedTree) printNode(node *ModuleNode, prefix string, lines *[]string) {
	// Format: [key] name (source_type) -> install_dir
	line := prefix
	if node.IsRoot() {
		line += SourceDisplayRoot
	} else {
		line += node.Key + " " + node.Name
	}

	if node.Source.Type != 0 {
		line += " (" + node.Source.Type.String() + ")"
	}

	if node.InstallDir != "" {
		line += " -> " + node.InstallDir
	}

	*lines = append(*lines, line)

	// Print children
	for i, child := range node.Children {
		var childPrefix string
		if i < len(node.Children)-1 {
			childPrefix = prefix + "├─"
		} else {
			childPrefix = prefix + "└─"
		}
		t.printNode(child, childPrefix, lines)
	}
}
