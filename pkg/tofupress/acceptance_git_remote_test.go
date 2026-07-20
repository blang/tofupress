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
	// The subdir must itself be a module (with .tf at its root): `tofupress module`
	// refuses a no-.tf subject (ADR-0002). The repo's //examples directory is a tree
	// of example modules (no .tf at examples/ root) — the `tofupress tree` case —
	// so point at a single example module that has its own main.tf.
	source := "github.com/terraform-aws-modules/terraform-aws-vpc//examples/complete?ref=v5.0.0"

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
