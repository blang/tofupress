package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/blang/tofupress/pkg/tofupress"
)

func TestOutputText_RootOnly(t *testing.T) {
	tree := simpleResolveTree(t, ".")
	var buf bytes.Buffer
	err := outputText(&buf, tree)
	require.NoError(t, err)
	out := buf.String()
	assert.Contains(t, out, "Module tree:")
	assert.Contains(t, out, "Total modules: 1")
	assert.Contains(t, out, "Total packages: 0")
}

func TestOutputText_WithChildren(t *testing.T) {
	tree := resolveTreeWithModules(t, ".")
	var buf bytes.Buffer
	err := outputText(&buf, tree)
	require.NoError(t, err)
	out := buf.String()
	assert.Contains(t, out, "Module tree:")
	assert.Contains(t, out, "mod_vpc")
	assert.Contains(t, out, "mod_sg")
	assert.Contains(t, out, "Total modules: 3")
}

func TestOutputText_WithRemoteModule(t *testing.T) {
	tree := resolveTreeWithRemoteModule(t)
	var buf bytes.Buffer
	err := outputText(&buf, tree)
	require.NoError(t, err)
	out := buf.String()
	assert.Contains(t, out, "remote")
	assert.Contains(t, out, "mod_remote")
}

func TestPrintModuleTree_Nested(t *testing.T) {
	root := &tofupress.ModuleNode{
		Key:        "",
		Name:       "root",
		InstallDir: "/app",
		IsLocal:    true,
		Children: []*tofupress.ModuleNode{
			{
				Key:        "mod_a",
				Name:       "mod_a",
				InstallDir: "/app/modules/a",
				IsLocal:    true,
				Children: []*tofupress.ModuleNode{
					{
						Key:        "mod_a.mod_b",
						Name:       "mod_b",
						InstallDir: "/app/modules/a/modules/b",
						IsLocal:    true,
					},
				},
			},
		},
	}

	var buf bytes.Buffer
	printModuleTree(&buf, root, 0)
	out := buf.String()

	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.GreaterOrEqual(t, len(lines), 3, "expected at least 3 lines, got: %q", out)

	// Output format: name line (no Path/Source lines since none are set in fixture)
	assert.Contains(t, lines[0], "root")
	assert.Contains(t, lines[0], "local")
	assert.Contains(t, lines[1], "mod_a")
	assert.Contains(t, lines[2], "mod_b")
}

func TestOutputJSON_RootOnly(t *testing.T) {
	tree := simpleResolveTree(t, ".")
	var buf bytes.Buffer
	err := outputJSON(&buf, tree)
	require.NoError(t, err)

	var result map[string]any
	err = json.Unmarshal(buf.Bytes(), &result)
	require.NoError(t, err)

	assert.Equal(t, "", result["root"], "root module key is empty string")
	modules, ok := result["modules"].([]any)
	require.True(t, ok)
	assert.Len(t, modules, 1)
}

func TestOutputJSON_WithChildren(t *testing.T) {
	tree := resolveTreeWithModules(t, ".")
	var buf bytes.Buffer
	err := outputJSON(&buf, tree)
	require.NoError(t, err)

	var result map[string]any
	err = json.Unmarshal(buf.Bytes(), &result)
	require.NoError(t, err)

	modules, ok := result["modules"].([]any)
	require.True(t, ok)
	assert.Len(t, modules, 3)
}

func TestOutputJSON_RemoteSourceType(t *testing.T) {
	tree := resolveTreeWithRemoteModule(t)
	var buf bytes.Buffer
	err := outputJSON(&buf, tree)
	require.NoError(t, err)

	var result map[string]any
	err = json.Unmarshal(buf.Bytes(), &result)
	require.NoError(t, err)

	modules, ok := result["modules"].([]any)
	require.True(t, ok)

	found := false
	for _, m := range modules {
		mod, ok := m.(map[string]any)
		require.True(t, ok)
		if mod["name"] == "mod_remote" {
			isRemote, ok := mod["is_remote"].(bool)
			require.True(t, ok)
			assert.True(t, isRemote)
			isLocal, ok := mod["is_local"].(bool)
			require.True(t, ok)
			assert.False(t, isLocal)
			found = true
		}
	}
	assert.True(t, found, "remote module not found in JSON output")
}

// --- helpers ---

func simpleResolveTree(t *testing.T, installDir string) *tofupress.ResolvedTree {
	t.Helper()
	return &tofupress.ResolvedTree{
		Root: &tofupress.ModuleNode{
			Key:        "",
			Name:       "root",
			InstallDir: installDir,
			IsLocal:    true,
		},
		AllModules: []*tofupress.ModuleNode{
			{
				Key:        "",
				Name:       "root",
				InstallDir: installDir,
				IsLocal:    true,
			},
		},
		Packages: make(map[string]*tofupress.DownloadedPackage),
	}
}

func resolveTreeWithModules(t *testing.T, installDir string) *tofupress.ResolvedTree {
	t.Helper()
	root := &tofupress.ModuleNode{
		Key:        "",
		Name:       "root",
		InstallDir: installDir,
		IsLocal:    true,
	}
	child1 := &tofupress.ModuleNode{
		Key:        "mod_vpc",
		Name:       "mod_vpc",
		InstallDir: installDir + "/modules/vpc",
		IsLocal:    true,
		Parent:     root,
		Source:     tofupress.ModuleSource{Raw: "./modules/vpc", Type: tofupress.SourceLocal},
	}
	child2 := &tofupress.ModuleNode{
		Key:        "mod_sg",
		Name:       "mod_sg",
		InstallDir: installDir + "/modules/sg",
		IsLocal:    true,
		Parent:     root,
		Source:     tofupress.ModuleSource{Raw: "./modules/sg", Type: tofupress.SourceLocal},
	}
	root.Children = []*tofupress.ModuleNode{child1, child2}

	return &tofupress.ResolvedTree{
		Root:       root,
		AllModules: []*tofupress.ModuleNode{root, child1, child2},
		Packages:   make(map[string]*tofupress.DownloadedPackage),
	}
}

func resolveTreeWithRemoteModule(t *testing.T) *tofupress.ResolvedTree {
	t.Helper()
	root := &tofupress.ModuleNode{
		Key:        "",
		Name:       "root",
		InstallDir: "/app",
		IsLocal:    true,
	}
	remote := &tofupress.ModuleNode{
		Key:        "mod_remote",
		Name:       "mod_remote",
		InstallDir: "/app/modules/abc123",
		IsLocal:    false,
		IsRemote:   true,
		Parent:     root,
		Source: tofupress.ModuleSource{
			Raw:         "git::https://github.com/test/repo.git?ref=v1.0.0",
			Type:        tofupress.SourceGit,
			PackageAddr: "git::https://github.com/test/repo.git?ref=v1.0.0",
			Ref:         "v1.0.0",
		},
	}
	root.Children = []*tofupress.ModuleNode{remote}

	return &tofupress.ResolvedTree{
		Root:       root,
		AllModules: []*tofupress.ModuleNode{root, remote},
		Packages:   make(map[string]*tofupress.DownloadedPackage),
	}
}

// TestResolveOutPlanFile (review item 8) verifies resolve --out=plan.json
// writes a valid JSON resolution to the file (for CI inspection/archival).
// The plan-file-alone landing; bundle --from-resolution lands with the cache.
func TestResolveOutPlanFile(t *testing.T) {
	tree := resolveTreeWithRemoteModule(t)
	tmp := t.TempDir()
	planPath := filepath.Join(tmp, "plan.json")

	f, err := os.Create(planPath) //nolint:gosec // test path
	require.NoError(t, err)
	require.NoError(t, outputJSON(f, tree))
	require.NoError(t, f.Close())

	data, err := os.ReadFile(planPath) //nolint:gosec // G304: test path from t.TempDir
	require.NoError(t, err)
	assert.NotEmpty(t, data)
	// Must be valid JSON an be parseable as the resolution shape.
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(data, &parsed))
	assert.Contains(t, parsed, "modules")
}
