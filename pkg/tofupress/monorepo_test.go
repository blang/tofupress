//go:build integration

package tofupress

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBinary_BundleMonorepoComplex validates TofuPress handles realistic monorepo structures
// with complex module references: relative paths, deep nesting, mixed local/remote, deduplication
func TestBinary_BundleMonorepoComplex(t *testing.T) {
	binary := buildBinary(t)

	// Use the monorepo fixture - resolve absolute path
	cwd, err := os.Getwd()
	require.NoError(t, err)

	fixtureDir := filepath.Join(cwd, "..", "..", "test-fixtures", "monorepo") + "//infra/environments/prod"

	// Verify fixture exists
	_, err = os.Stat(fixtureDir)
	require.NoError(t, err, "fixture directory should exist: %s", fixtureDir)

	// Create temp output file
	outputDir := t.TempDir()
	outputFile := filepath.Join(outputDir, "bundle.tar.gz")

	// Bundle the prod environment
	cmd := exec.Command(binary, "bundle", fixtureDir, outputFile) //nolint:gosec // G204: subprocess is intentional
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "binary bundle should succeed: %s", string(output))

	// Verify output file was created
	info, err := os.Stat(outputFile)
	require.NoError(t, err, "bundle file should exist")
	assert.Greater(t, info.Size(), int64(0), "bundle file should not be empty")
	t.Logf("Bundle size: %d bytes", info.Size())

	// Extract and validate bundle structure
	extractDir := t.TempDir()
	extractTarGz(t, outputFile, extractDir)

	// Verify main.tf exists in the bundle (archived from PackageRoot for //subdir inputs)
	// For subdir inputs (e.g., monorepo//infra/environments/prod), the archive preserves
	// the full package structure: infra/environments/prod/main.tf
	mainTf := filepath.Join(extractDir, "infra", "environments", "prod", "main.tf")
	content, err := os.ReadFile(mainTf)
	require.NoError(t, err, "main.tf should be readable")

	mainContent := string(content)
	t.Logf("Root main.tf content:\n%s", mainContent)

	// All module sources should be relative (start with ./ or ../)
	lines := strings.Split(mainContent, "\n")
	for _, line := range lines {
		if strings.Contains(line, "source") && strings.Contains(line, "=") {
			// Extract the source value
			parts := strings.Split(line, "=")
			if len(parts) == 2 {
				source := strings.TrimSpace(parts[1])
				source = strings.Trim(source, "\"")

				// Should be relative path (starts with ./ or ../)
				assert.True(t,
					strings.HasPrefix(source, "./") || strings.HasPrefix(source, "../"),
					"source should be relative path, got: %s", source)
			}
		}
	}

	// Verify sourcetree directory does NOT exist (no remote packages downloaded)
	// The monorepo fixture has only local modules with no remote dependencies.
	sourcetreeDir := filepath.Join(extractDir, "infra", "environments", "prod", "modules")
	_, err = os.Stat(sourcetreeDir)
	assert.True(t, os.IsNotExist(err), "sourcetree should not exist for local-only modules: %v", err)
}

// TestBinary_BundleMonorepoMultipleEnvs validates bundling different environments
// from the same monorepo works correctly
func TestBinary_BundleMonorepoMultipleEnvs(t *testing.T) {
	binary := buildBinary(t)

	cwd, err := os.Getwd()
	require.NoError(t, err)

	testCases := []struct {
		name       string
		fixtureDir string
	}{
		{
			name:       "staging environment",
			fixtureDir: filepath.Join(cwd, "..", "..", "test-fixtures", "monorepo") + "//infra/environments/staging",
		},
		{
			name:       "app1",
			fixtureDir: filepath.Join(cwd, "..", "..", "test-fixtures", "monorepo") + "//apps/app1",
		},
		{
			name:       "platform dev overlay",
			fixtureDir: filepath.Join(cwd, "..", "..", "test-fixtures", "monorepo") + "//platform/overlays/dev",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Verify fixture exists
			_, err := os.Stat(tc.fixtureDir)
			require.NoError(t, err, "fixture directory should exist: %s", tc.fixtureDir)

			// Create temp output file
			outputDir := t.TempDir()
			outputFile := filepath.Join(outputDir, "bundle.tar.gz")

			// Bundle
			cmd := exec.Command(binary, "bundle", tc.fixtureDir, outputFile) //nolint:gosec // G204: subprocess is intentional
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, "binary bundle should succeed: %s", string(output))

			// Verify output file was created
			info, err := os.Stat(outputFile)
			require.NoError(t, err, "bundle file should exist")
			assert.Greater(t, info.Size(), int64(0), "bundle file should not be empty")

			t.Logf("Successfully bundled %s (%d bytes)", tc.name, info.Size())
		})
	}
}

// extractTarGz is defined in bundler_test.go
