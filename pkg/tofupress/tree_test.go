//nolint:govet // test structs prioritize readability over memory layout
package tofupress

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModuleSource_Local(t *testing.T) {
	src := ModuleSource{
		Raw:  "./modules/mod1",
		Type: SourceLocal,
	}

	assert.Equal(t, SourceLocal, src.Type)
	assert.Equal(t, "./modules/mod1", src.Raw)
	assert.Empty(t, src.PackageAddr)
	assert.Empty(t, src.SubDir)
}

func TestModuleSource_Git(t *testing.T) {
	src := ModuleSource{
		Raw:         "git::https://github.com/user/repo.git//modules/vpc?ref=v1.0",
		Type:        SourceGit,
		PackageAddr: "git::https://github.com/user/repo.git?ref=v1.0",
		SubDir:      "modules/vpc",
		Ref:         "v1.0",
	}

	assert.Equal(t, SourceGit, src.Type)
	assert.Equal(t, "git::https://github.com/user/repo.git?ref=v1.0", src.PackageAddr)
	assert.Equal(t, "modules/vpc", src.SubDir)
	assert.Equal(t, "v1.0", src.Ref)
}

func TestModuleSource_Registry(t *testing.T) {
	src := ModuleSource{
		Raw:  "hashicorp/consul/aws",
		Type: SourceRegistry,
		Ref:  "1.0.0",
	}

	assert.Equal(t, SourceRegistry, src.Type)
	assert.Equal(t, "hashicorp/consul/aws", src.Raw)
	assert.Equal(t, "1.0.0", src.Ref)
}

func TestModuleNode_Root(t *testing.T) {
	root := &ModuleNode{
		Key:        "",
		Name:       "root",
		InstallDir: "/path/to/root",
		IsLocal:    true,
	}

	assert.Empty(t, root.Key)
	assert.Equal(t, "root", root.Name)
	assert.Nil(t, root.Parent)
	assert.Empty(t, root.Children)
	assert.True(t, root.IsLocal)
	assert.False(t, root.IsRemote)
}

func TestModuleNode_WithChildren(t *testing.T) {
	root := &ModuleNode{
		Key:        "",
		Name:       "root",
		InstallDir: "/root",
		IsLocal:    true,
	}

	child1 := &ModuleNode{
		Key:        "sub1",
		Name:       "sub1",
		Parent:     root,
		InstallDir: "/root/modules/mod1",
		IsLocal:    true,
	}

	child2 := &ModuleNode{
		Key:        "sub2",
		Name:       "sub2",
		Parent:     root,
		InstallDir: "/root/modules/abc123",
		IsRemote:   true,
	}

	root.Children = []*ModuleNode{child1, child2}

	assert.Len(t, root.Children, 2)
	assert.Equal(t, "sub1", root.Children[0].Name)
	assert.Equal(t, "sub2", root.Children[1].Name)
	assert.Equal(t, root, child1.Parent)
	assert.Equal(t, root, child2.Parent)
}

func TestModuleNode_NestedHierarchy(t *testing.T) {
	root := &ModuleNode{
		Key:  "",
		Name: "root",
	}

	sub1 := &ModuleNode{
		Key:    "sub1",
		Name:   "sub1",
		Parent: root,
	}

	sub1ext := &ModuleNode{
		Key:    "sub1.subext",
		Name:   "subext",
		Parent: sub1,
	}

	sub1extmod1 := &ModuleNode{
		Key:    "sub1.subext.mod1",
		Name:   "mod1",
		Parent: sub1ext,
	}

	root.Children = []*ModuleNode{sub1}
	sub1.Children = []*ModuleNode{sub1ext}
	sub1ext.Children = []*ModuleNode{sub1extmod1}

	// Verify hierarchy
	assert.Equal(t, root, sub1.Parent)
	assert.Equal(t, sub1, sub1ext.Parent)
	assert.Equal(t, sub1ext, sub1extmod1.Parent)

	// Verify keys
	assert.Equal(t, "sub1", sub1.Key)
	assert.Equal(t, "sub1.subext", sub1ext.Key)
	assert.Equal(t, "sub1.subext.mod1", sub1extmod1.Key)
}

func TestDownloadedPackage(t *testing.T) {
	pkg := &DownloadedPackage{
		PackageAddr: "git::https://github.com/user/repo.git?ref=v1.0",
		LocalDir:    "/tmp/vendor/abc123",
	}

	assert.Equal(t, "git::https://github.com/user/repo.git?ref=v1.0", pkg.PackageAddr)
	assert.Equal(t, "/tmp/vendor/abc123", pkg.LocalDir)
}

func TestResolvedTree_Empty(t *testing.T) {
	tree := &ResolvedTree{
		Root: &ModuleNode{
			Key:  "",
			Name: "root",
		},
		AllModules: []*ModuleNode{},
		Packages:   make(map[string]*DownloadedPackage),
		VendorDir:  "/tmp/vendor",
	}

	require.NotNil(t, tree.Root)
	assert.Empty(t, tree.AllModules)
	assert.Empty(t, tree.Packages)
	assert.Equal(t, "/tmp/vendor", tree.VendorDir)
}

func TestResolvedTree_WithModules(t *testing.T) {
	root := &ModuleNode{
		Key:  "",
		Name: "root",
	}

	mod1 := &ModuleNode{
		Key:        "sub1",
		Name:       "sub1",
		Parent:     root,
		InstallDir: "/root/modules/mod1",
		IsLocal:    true,
	}

	mod2 := &ModuleNode{
		Key:        "sub2",
		Name:       "sub2",
		Parent:     root,
		InstallDir: "/tmp/vendor/abc123/modules/vpc",
		IsRemote:   true,
	}

	tree := &ResolvedTree{
		Root:       root,
		AllModules: []*ModuleNode{root, mod1, mod2},
		Packages: map[string]*DownloadedPackage{
			"git::https://github.com/user/repo.git?ref=v1.0": {
				PackageAddr: "git::https://github.com/user/repo.git?ref=v1.0",
				LocalDir:    "/tmp/vendor/abc123",
			},
		},
		VendorDir: "/tmp/vendor",
	}

	assert.Len(t, tree.AllModules, 3)
	assert.Len(t, tree.Packages, 1)
	assert.Equal(t, root, tree.Root)

	// Verify package exists
	pkg, exists := tree.Packages["git::https://github.com/user/repo.git?ref=v1.0"]
	require.True(t, exists)
	assert.Equal(t, "/tmp/vendor/abc123", pkg.LocalDir)
}

func TestMakeKey(t *testing.T) {
	tests := []struct {
		name       string
		parentKey  string
		moduleName string
		want       string
	}{
		{
			name:       "root level",
			parentKey:  "",
			moduleName: "sub1",
			want:       "sub1",
		},
		{
			name:       "one level deep",
			parentKey:  "sub1",
			moduleName: "subext",
			want:       "sub1.subext",
		},
		{
			name:       "two levels deep",
			parentKey:  "sub1.subext",
			moduleName: "mod1",
			want:       "sub1.subext.mod1",
		},
		{
			name:       "three levels deep",
			parentKey:  "sub3.subext.mod2",
			moduleName: "subext",
			want:       "sub3.subext.mod2.subext",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MakeKey(tt.parentKey, tt.moduleName)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestModuleNode_FindByKey(t *testing.T) {
	root := &ModuleNode{Key: "", Name: "root"}
	mod1 := &ModuleNode{Key: "sub1", Name: "sub1", Parent: root}
	mod2 := &ModuleNode{Key: "sub1.subext", Name: "subext", Parent: mod1}
	mod3 := &ModuleNode{Key: "sub2", Name: "sub2", Parent: root}

	root.Children = []*ModuleNode{mod1, mod3}
	mod1.Children = []*ModuleNode{mod2}

	tests := []struct {
		name     string
		key      string
		wantNode *ModuleNode
	}{
		{"find root", "", root},
		{"find sub1", "sub1", mod1},
		{"find sub1.subext", "sub1.subext", mod2},
		{"find sub2", "sub2", mod3},
		{"not found", "nonexistent", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := root.FindByKey(tt.key)
			assert.Equal(t, tt.wantNode, got)
		})
	}
}

// buildTestTree creates a reusable 3-level test tree:
//
//	root
//	├── child1
//	│   └── grandchild1
//	└── child2
func buildTestTree() *ModuleNode {
	root := &ModuleNode{Key: "", Name: "root", InstallDir: "/root"}
	child1 := &ModuleNode{Key: "child1", Name: "child1", Parent: root, InstallDir: "/root/child1", IsLocal: true}
	child2 := &ModuleNode{Key: "child2", Name: "child2", Parent: root, InstallDir: "/root/child2", IsRemote: true}
	grandchild1 := &ModuleNode{Key: "child1.grandchild1", Name: "grandchild1", Parent: child1, InstallDir: "/root/child1/grandchild1", IsLocal: true}

	root.Children = []*ModuleNode{child1, child2}
	child1.Children = []*ModuleNode{grandchild1}

	return root
}

func TestModuleNode_Depth(t *testing.T) {
	root := buildTestTree()
	child1 := root.Children[0]
	grandchild1 := child1.Children[0]

	assert.Equal(t, 0, root.Depth())
	assert.Equal(t, 1, child1.Depth())
	assert.Equal(t, 2, grandchild1.Depth())
}

func TestModuleNode_Path(t *testing.T) {
	root := buildTestTree()
	child1 := root.Children[0]
	grandchild1 := child1.Children[0]

	assert.Equal(t, []string{"root"}, root.Path())
	assert.Equal(t, []string{"root", "child1"}, child1.Path())
	assert.Equal(t, []string{"root", "child1", "grandchild1"}, grandchild1.Path())
}

func TestModuleNode_String(t *testing.T) {
	root := buildTestTree()
	child1 := root.Children[0]

	assert.Equal(t, "", root.String())
	assert.Equal(t, "child1", child1.String())

	// nil receiver
	var nilNode *ModuleNode
	assert.Equal(t, "<nil>", nilNode.String())
}

func TestModuleNode_HasChildren(t *testing.T) {
	root := buildTestTree()
	child2 := root.Children[1]

	assert.True(t, root.HasChildren())
	assert.True(t, root.Children[0].HasChildren())
	assert.False(t, child2.HasChildren())
}

func TestModuleNode_ChildCount(t *testing.T) {
	root := buildTestTree()

	assert.Equal(t, 2, root.ChildCount())
	assert.Equal(t, 1, root.Children[0].ChildCount())
	assert.Equal(t, 0, root.Children[1].ChildCount())
}

func TestModuleNode_TotalDescendants(t *testing.T) {
	root := buildTestTree()

	assert.Equal(t, 3, root.TotalDescendants())
	assert.Equal(t, 1, root.Children[0].TotalDescendants())
	assert.Equal(t, 0, root.Children[1].TotalDescendants())
}

func TestModuleNode_IsRoot(t *testing.T) {
	root := buildTestTree()

	assert.True(t, root.IsRoot())
	assert.False(t, root.Children[0].IsRoot())
}

func TestModuleNode_RootMethod(t *testing.T) {
	tree := buildTestTree()
	grandchild1 := tree.Children[0].Children[0]

	assert.Equal(t, tree, tree.Root())
	assert.Equal(t, tree, grandchild1.Root())
}

func TestModuleNode_Siblings(t *testing.T) {
	tree := buildTestTree()
	child1 := tree.Children[0]
	child2 := tree.Children[1]

	siblings := child1.Siblings()
	assert.Len(t, siblings, 1)
	assert.Equal(t, child2, siblings[0])

	siblings = child2.Siblings()
	assert.Len(t, siblings, 1)
	assert.Equal(t, child1, siblings[0])

	assert.Nil(t, tree.Siblings())
}

func TestModuleNode_Ancestors(t *testing.T) {
	tree := buildTestTree()
	grandchild1 := tree.Children[0].Children[0]

	assert.Nil(t, tree.Ancestors())

	child1 := tree.Children[0]
	ancestors := child1.Ancestors()
	assert.Len(t, ancestors, 1)
	assert.Equal(t, tree, ancestors[0])

	ancestors = grandchild1.Ancestors()
	assert.Len(t, ancestors, 2)
	assert.Equal(t, tree, ancestors[0])
	assert.Equal(t, child1, ancestors[1])
}

func TestModuleNode_Walk(t *testing.T) {
	tree := buildTestTree()

	var keys []string
	tree.Walk(func(n *ModuleNode) {
		keys = append(keys, n.Key)
	})

	assert.Equal(t, []string{"", "child1", "child1.grandchild1", "child2"}, keys)
}

func TestModuleNode_FindByName(t *testing.T) {
	tree := buildTestTree()

	found := tree.FindByName("child1")
	require.NotNil(t, found)
	assert.Equal(t, "child1", found.Name)

	found = tree.FindByName("child2")
	require.NotNil(t, found)
	assert.Equal(t, "child2", found.Name)

	found = tree.FindByName("grandchild1")
	assert.Nil(t, found)

	found = tree.FindByName("nonexistent")
	assert.Nil(t, found)
}

func TestModuleNode_FindAllByName(t *testing.T) {
	tree := buildTestTree()

	found := tree.FindAllByName("child1")
	assert.Len(t, found, 1)

	found = tree.FindAllByName("grandchild1")
	assert.Len(t, found, 1)

	found = tree.FindAllByName("root")
	assert.Len(t, found, 1)

	found = tree.FindAllByName("nonexistent")
	assert.Empty(t, found)
}

func TestResolvedTree_Contains(t *testing.T) {
	root := buildTestTree()
	tree := &ResolvedTree{
		Root:       root,
		AllModules: []*ModuleNode{root, root.Children[0], root.Children[0].Children[0], root.Children[1]},
	}

	assert.True(t, tree.Contains(""))
	assert.True(t, tree.Contains("child1"))
	assert.True(t, tree.Contains("child1.grandchild1"))
	assert.True(t, tree.Contains("child2"))
	assert.False(t, tree.Contains("nonexistent"))
}

func TestResolvedTree_Find(t *testing.T) {
	root := buildTestTree()
	child2 := root.Children[1]
	tree := &ResolvedTree{
		Root:       root,
		AllModules: []*ModuleNode{root, root.Children[0], root.Children[0].Children[0], child2},
	}

	assert.Equal(t, root, tree.Find(""))
	assert.Equal(t, child2, tree.Find("child2"))
	assert.Nil(t, tree.Find("nonexistent"))
}

func TestResolvedTree_ModuleCount(t *testing.T) {
	root := buildTestTree()
	tree := &ResolvedTree{
		Root:       root,
		AllModules: []*ModuleNode{root, root.Children[0], root.Children[1]},
	}

	assert.Equal(t, 3, tree.ModuleCount())
}

func TestResolvedTree_PackageCount(t *testing.T) {
	tree := &ResolvedTree{
		Root:     &ModuleNode{},
		Packages: map[string]*DownloadedPackage{"a": {}, "b": {}},
	}

	assert.Equal(t, 2, tree.PackageCount())

	tree.Packages = make(map[string]*DownloadedPackage)
	assert.Equal(t, 0, tree.PackageCount())
}

func TestResolvedTree_LocalModuleCount(t *testing.T) {
	root := buildTestTree()
	tree := &ResolvedTree{
		Root:       root,
		AllModules: []*ModuleNode{root, root.Children[0], root.Children[0].Children[0], root.Children[1]},
	}

	assert.Equal(t, 2, tree.LocalModuleCount())
}

func TestResolvedTree_RemoteModuleCount(t *testing.T) {
	root := buildTestTree()
	tree := &ResolvedTree{
		Root:       root,
		AllModules: []*ModuleNode{root, root.Children[0], root.Children[0].Children[0], root.Children[1]},
	}

	assert.Equal(t, 1, tree.RemoteModuleCount())
}

func TestResolvedTree_PrintTree(t *testing.T) {
	root := buildTestTree()
	tree := &ResolvedTree{
		Root:       root,
		AllModules: []*ModuleNode{root, root.Children[0], root.Children[0].Children[0], root.Children[1]},
	}

	output := tree.PrintTree()
	assert.Contains(t, output, "root")
	assert.Contains(t, output, "child1")
	assert.Contains(t, output, "child2")
	assert.Contains(t, output, "grandchild1")

	emptyTree := &ResolvedTree{}
	assert.Equal(t, "<empty tree>", emptyTree.PrintTree())
}
