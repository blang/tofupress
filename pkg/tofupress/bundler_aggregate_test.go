//nolint:gosec,gocyclo // test file uses standard permissions and has complex test logic
package tofupress

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
  source = "./sourcetree/abc123"
}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create sourcetree with an inner package
	innerPackageDir := filepath.Join(pressedModuleDir, "sourcetree", "abc123")
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
	bundleErr := bundler.Bundle(tree, outputPath)
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

	// Verify that the inner package is in the root sourcetree (flattened)
	// The inner package should NOT be in a nested sourcetree
	flattenedPackageDir := filepath.Join(extractDir, "sourcetree", "abc123")
	if _, statErr := os.Stat(flattenedPackageDir); os.IsNotExist(statErr) {
		t.Errorf("Inner package should be flattened to root sourcetree at %s", flattenedPackageDir)
	}

	// Verify that there's no nested sourcetree
	nestedSourcetreePath := filepath.Join(extractDir, "pressed", "sourcetree")
	if _, statErr := os.Stat(nestedSourcetreePath); !os.IsNotExist(statErr) {
		t.Errorf("Should not have nested sourcetree at %s", nestedSourcetreePath)
	}

	// Verify that the pressed module's source was rewritten to point to the flattened location
	pressedModuleMainTF := filepath.Join(extractDir, "pressed", "main.tf")
	content, readErr := os.ReadFile(pressedModuleMainTF)
	if readErr != nil {
		t.Fatalf("Failed to read pressed module main.tf: %v", readErr)
	}

	// The source should be rewritten to point to the flattened sourcetree
	if !strings.Contains(string(content), "./../sourcetree/abc123") {
		t.Errorf("Pressed module source should be rewritten to point to flattened sourcetree, got: %s", string(content))
	}
}
