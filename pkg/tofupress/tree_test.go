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
		InstallDir: "/root/sourcetree/abc123",
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
