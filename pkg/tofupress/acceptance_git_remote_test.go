//go:build integration

//nolint:gosec // integration tests execute test binary and access network
package tofupress

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAcceptance_Git_HostShorthand(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network-dependent host-shorthand test in short mode")
	}

	bin := buildBinary(t)
	source := "github.com/terraform-aws-modules/terraform-aws-vpc?ref=v5.0.0"

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, source, artifact, "--format=zip")

	validateArchiveWithAllTools(t, serveArtifact(t, artifact))
}

func TestAcceptance_Git_SubdirHostShorthandRefusesUnpreservablePivot(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network-dependent host-shorthand subdir test in short mode")
	}

	bin := buildBinary(t)
	// The selected example is a module, but it calls the repository root via
	// ../... Both cannot occupy archive root after a module pivot, so the command
	// must recognize the host-shorthand subdir and then refuse semantic corruption.
	source := "github.com/terraform-aws-modules/terraform-aws-vpc//examples/complete?ref=v5.0.0"

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	_, stderr := runTofuPressBundleExpectFailure(t, bin, source, artifact, "--format=zip")

	assert.Contains(t, stderr, "cannot preserve")
	assert.Contains(t, stderr, "tofupress tree")
	assert.NoFileExists(t, artifact)
}

func TestAcceptance_Git_LocalVsHostShorthand(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network-dependent local-vs-host test in short mode")
	}

	bin := buildBinary(t)
	source := "github.com/terraform-aws-modules/terraform-aws-vpc?ref=v5.0.0"

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, source, artifact, "--format=zip")

	validateArchiveWithAllTools(t, serveArtifact(t, artifact))
}
