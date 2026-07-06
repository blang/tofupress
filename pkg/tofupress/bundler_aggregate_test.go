//nolint:gosec,gocyclo // test file uses standard permissions and has complex test logic
package tofupress

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBundler_AggregatePressedSubModules(t *testing.T) {
	// Create a temporary directory for testing
	tmpDir := t.TempDir()

	// Create a pressed module (a bundle with sourcetree)
	pressedModuleDir := filepath.Join(tmpDir, "pressed-module")
	if err := os.MkdirAll(pressedModuleDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create main.tf for the pressed module
	pressedMainTF := filepath.Join(pressedModuleDir, "main.tf")
	if err := os.WriteFile(pressedMainTF, []byte(`
module "inner" {
  source = "./`+defaultVendorDir+`/abc123"
}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create the pressed module's own vendor dir with an inner package
	innerPackageDir := filepath.Join(pressedModuleDir, defaultVendorDir, "abc123")
	if err := os.MkdirAll(innerPackageDir, 0o755); err != nil {
		t.Fatal(err)
	}

	innerMainTF := filepath.Join(innerPackageDir, "main.tf")
	if err := os.WriteFile(innerMainTF, []byte(`
resource "null_resource" "inner" {}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create root module that references the pressed module
	rootDir := filepath.Join(tmpDir, "root")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatal(err)
	}

	rootMainTF := filepath.Join(rootDir, "main.tf")
	if err := os.WriteFile(rootMainTF, []byte(`
module "pressed" {
  source = "../pressed-module"
}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Resolve the tree
	resolver := NewResolver()
	tree, resolveErr := resolver.Resolve(context.Background(), rootDir)
	if resolveErr != nil {
		t.Fatalf("Failed to resolve: %v", resolveErr)
	}

	// Bundle with tar.gz format
	outputPath := filepath.Join(tmpDir, "output.tar.gz")
	bundler := NewBundler(BundleFormatTarGZ)
	bundleErr := bundler.Bundle(context.Background(), tree, outputPath)
	if bundleErr != nil {
		t.Fatalf("Failed to bundle: %v", bundleErr)
	}

	// Extract and verify the bundle structure
	extractDir := filepath.Join(tmpDir, "extracted")
	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		t.Fatal(err)
	}

	extractTarGz(t, outputPath, extractDir)

	// Debug: list all files in extracted directory
	t.Log("Extracted bundle contents:")
	walkErr := filepath.Walk(extractDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relPath, _ := filepath.Rel(extractDir, path)
		if !info.IsDir() {
			t.Logf("  %s", relPath)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("Failed to walk extracted directory: %v", walkErr)
	}

	// Verify that the inner package is flattened into the root vendor dir
	// The inner package should NOT remain in a nested vendor dir.
	flattenedPackageDir := filepath.Join(extractDir, defaultVendorDir, "abc123")
	if _, statErr := os.Stat(flattenedPackageDir); os.IsNotExist(statErr) {
		t.Errorf("Inner package should be flattened to root vendor dir at %s", flattenedPackageDir)
	}

	// Verify that there's no nested vendor dir inside the pressed module
	nestedSourcetreePath := filepath.Join(extractDir, "pressed", defaultVendorDir)
	if _, statErr := os.Stat(nestedSourcetreePath); !os.IsNotExist(statErr) {
		t.Errorf("Should not have nested vendor dir at %s", nestedSourcetreePath)
	}

	// Verify that the pressed module's source was rewritten to point to the flattened location
	pressedModuleMainTF := filepath.Join(extractDir, "pressed", "main.tf")
	content, readErr := os.ReadFile(pressedModuleMainTF)
	if readErr != nil {
		t.Fatalf("Failed to read pressed module main.tf: %v", readErr)
	}

	// The source should be rewritten to point to the flattened vendor dir
	if !strings.Contains(string(content), "./../"+defaultVendorDir+"/abc123") {
		t.Errorf("Pressed module source should be rewritten to point to flattened vendor dir, got: %s", string(content))
	}
}

// TestBundler_AggregationPreservesRootInstallDir is the library contract test
// for review item 5 / F9: a library caller passing a real on-disk tree must
// get tree.Root.InstallDir back byte-identical after Bundle. Aggregation must
// operate entirely within the staging dir, never touching the caller's tree.
func TestBundler_AggregationPreservesRootInstallDir(t *testing.T) {
	tmpDir := t.TempDir()

	// Pressed module: has its own _vendor with an inner package.
	pressedDir := filepath.Join(tmpDir, "pressed")
	_ = os.MkdirAll(filepath.Join(pressedDir, defaultVendorDir, "abc123"), 0o755)
	_ = os.WriteFile(filepath.Join(pressedDir, "main.tf"),
		[]byte(`module "inner" { source = "./`+defaultVendorDir+`/abc123" }`), 0o644)
	_ = os.WriteFile(filepath.Join(pressedDir, defaultVendorDir, "abc123", "main.tf"),
		[]byte(`resource "null_resource" "inner" {}`), 0o644)

	// Root references the pressed module as a sibling.
	rootDir := filepath.Join(tmpDir, "root")
	_ = os.MkdirAll(rootDir, 0o755)
	_ = os.WriteFile(filepath.Join(rootDir, "main.tf"),
		[]byte(`module "pressed" { source = "../pressed" }`), 0o644)

	// Snapshot the entire caller tree (root + pressed sibling) BEFORE bundling.
	wantSnapshot := snapshotTree(t, tmpDir)

	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), rootDir)
	require.NoError(t, err, "Failed to resolve")

	bundler := NewBundler(BundleFormatTarGZ)
	bundlePath := filepath.Join(t.TempDir(), "out.tar.gz")
	require.NoError(t, bundler.Bundle(context.Background(), tree, bundlePath))

	// Aggregation must have written nothing into the caller's tree. Re-snapshot
	// and require byte-identical contents.
	gotSnapshot := snapshotTree(t, tmpDir)
	require.Equal(t, wantSnapshot, gotSnapshot,
		"Bundle must not modify the caller's tree (review item 5 / F9): "+
			"non-destructive aggregation contract violated")
}

// snapshotTree walks dir and returns a deterministic hash of its contents
// (path → file bytes), so two snapshots can be diffed for byte-identical trees.
func snapshotTree(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := make(map[string][]byte)
	walkErr := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		out[filepath.ToSlash(rel)] = data
		return nil
	})
	require.NoError(t, walkErr, "failed to snapshot tree at %s", dir)
	return out
}
