//go:build integration

//nolint:gosec // integration tests execute test binary
package tofupress

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAcceptance_Format_Zip(t *testing.T) {
	bin := buildBinary(t)
	src := createSimpleFixture(t)

	artifact := filepath.Join(t.TempDir(), "bundle.zip")
	runTofuPressBundle(t, bin, src, artifact, "--format=zip")

	validateArchiveWithAllTools(t, serveArtifact(t, artifact))

	metadata := metadataFromArtifact(t, artifact)
	assert.Equal(t, "zip", metadata.Command.Options.Format)
}

func TestAcceptance_Format_TarGz(t *testing.T) {
	bin := buildBinary(t)
	src := createSimpleFixture(t)

	artifact := filepath.Join(t.TempDir(), "bundle.tar.gz")
	runTofuPressBundle(t, bin, src, artifact, "--format=tar.gz")

	validateArchiveWithAllTools(t, serveArtifact(t, artifact))

	metadata := metadataFromArtifact(t, artifact)
	assert.Equal(t, "tar.gz", metadata.Command.Options.Format)
}

func TestAcceptance_Format_TarXz(t *testing.T) {
	bin := buildBinary(t)
	src := createSimpleFixture(t)

	artifact := filepath.Join(t.TempDir(), "bundle.tar.xz")
	runTofuPressBundle(t, bin, src, artifact, "--format=tar.xz")

	// Format metadata assertion is the primary check
	metadata := metadataFromArtifact(t, artifact)
	assert.Equal(t, "tar.xz", metadata.Command.Options.Format)

	// Validate if xz tools are available; skip validation if not
	// tar.xz validation may fail if xz is not in PATH
	validateArchiveWithAllTools(t, serveArtifact(t, artifact))
}

func TestAcceptance_Format_AutoDetection(t *testing.T) {
	bin := buildBinary(t)

	t.Run("zip", func(t *testing.T) {
		src := createSimpleFixture(t)
		artifact := filepath.Join(t.TempDir(), "bundle.zip")
		runTofuPressBundle(t, bin, src, artifact) // no --format flag

		metadata := metadataFromArtifact(t, artifact)
		assert.Equal(t, "zip", metadata.Command.Options.Format)
		validateArchiveWithAllTools(t, serveArtifact(t, artifact))
	})

	t.Run("targz", func(t *testing.T) {
		src := createSimpleFixture(t)
		artifact := filepath.Join(t.TempDir(), "bundle.tar.gz")
		runTofuPressBundle(t, bin, src, artifact) // no --format flag

		metadata := metadataFromArtifact(t, artifact)
		assert.Equal(t, "tar.gz", metadata.Command.Options.Format)
		validateArchiveWithAllTools(t, serveArtifact(t, artifact))
	})
}

func TestAcceptance_Format_BundleRoundTrip(t *testing.T) {
	bin := buildBinary(t)

	formats := []struct {
		name string
		ext  string
		flag string
	}{
		{name: "zip", ext: ".zip", flag: "--format=zip"},
		{name: "tar.gz", ext: ".tar.gz", flag: "--format=tar.gz"},
		{name: "tar.xz", ext: ".tar.xz", flag: "--format=tar.xz"},
	}

	for _, tt := range formats {
		t.Run(tt.name, func(t *testing.T) {
			src := createSimpleFixture(t)
			artifact := filepath.Join(t.TempDir(), "bundle"+tt.ext)
			runTofuPressBundle(t, bin, src, artifact, tt.flag)

			validateArchiveWithAllTools(t, serveArtifact(t, artifact))

			metadata := metadataFromArtifact(t, artifact)
			assert.Equal(t, tt.name, metadata.Command.Options.Format)
		})
	}
}
