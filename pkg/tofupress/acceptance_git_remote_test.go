//go:build integration

//nolint:gosec // integration tests execute test binary and access network
package tofupress

import (
	"path/filepath"
	"testing"
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

func TestAcceptance_Git_SubdirHostShorthand(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network-dependent host-shorthand subdir test in short mode")
	}

	bin := buildBinary(t)
	source := "github.com/terraform-aws-modules/terraform-aws-vpc?ref=v5.0.0//examples"

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, source, artifact, "--format=zip")

	validateArchiveWithAllTools(t, serveArtifact(t, artifact))
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
