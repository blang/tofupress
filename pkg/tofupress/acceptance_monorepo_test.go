//go:build integration

//nolint:gosec // integration tests execute test binary and use local paths
package tofupress

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// monorepoFixtureBase returns the absolute path to the monorepo test fixture.
func monorepoFixtureBase(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	require.NoError(t, err)
	return filepath.Join(cwd, "..", "..", "test-fixtures", "monorepo")
}

// monorepoFixture returns the path to a monorepo subdir, verifying the base exists.
func monorepoFixture(t *testing.T, subpath string) string {
	t.Helper()
	base := monorepoFixtureBase(t)
	_, err := os.Stat(base)
	require.NoError(t, err, "monorepo fixture base must exist: %s", base)
	full := base + "//" + subpath
	_, err = os.Stat(full)
	require.NoError(t, err, "monorepo fixture subdir must exist: %s", full)
	return full
}

func TestAcceptance_Monorepo_SubdirSyntax(t *testing.T) {
	bin := buildBinary(t)
	fixtureDir := monorepoFixture(t, "infra/environments/prod")

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, fixtureDir, artifact, "--format=zip")

	// //subdir bundles ship the entry module at the unpacked archive root, so
	// the consumer runs `tofu init` + `tofu validate` directly there — no
	// consumer wrapper, no `cd` (review item 2).
	consumeArchiveAtRootWithAllTools(t, artifact)
}

func TestAcceptance_Monorepo_SiblingRefs(t *testing.T) {
	bin := buildBinary(t)
	fixtureDir := monorepoFixture(t, "platform/overlays/dev")

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, fixtureDir, artifact, "--format=zip")

	consumeArchiveAtRootWithAllTools(t, artifact)
}

func TestAcceptance_Monorepo_DeepSubdir(t *testing.T) {
	bin := buildBinary(t)
	fixtureDir := monorepoFixture(t, "apps/app3")

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, fixtureDir, artifact, "--format=zip")

	consumeArchiveAtRootWithAllTools(t, artifact)
}

func TestAcceptance_Monorepo_MultipleEnvs(t *testing.T) {
	bin := buildBinary(t)

	testCases := []struct {
		name    string
		subpath string
	}{
		{name: "staging", subpath: "infra/environments/staging"},
		{name: "app1", subpath: "apps/app1"},
		{name: "prod_overlay", subpath: "platform/overlays/prod"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fixtureDir := monorepoFixture(t, tc.subpath)
			artifact := filepath.Join(t.TempDir(), "bundle.zip")
			runTofuPressBundle(t, bin, fixtureDir, artifact, "--format=zip")
			consumeArchiveAtRootWithAllTools(t, artifact)
		})
	}
}

func TestAcceptance_Monorepo_BundleOciCompliant(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping OCI-compliant monorepo test in short mode")
	}

	bin := buildBinary(t)
	fixtureDir := monorepoFixture(t, "infra/environments/prod")

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, fixtureDir, artifact, "--oci-compliant")

	consumeArchiveAtRootWithAllTools(t, artifact)
}
