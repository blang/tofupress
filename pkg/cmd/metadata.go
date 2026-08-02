package cmd

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blang/tofupress/pkg/tofupress"
)

var metadataCmd = &cobra.Command{
	Use:   "metadata <artifact|oci://registry/repo?tag=v1>",
	Short: "Print metadata embedded in a TofuPress artifact",
	Long: `Reads and displays metadata (meta.json) from a TofuPress artifact.

Supports local archive files (.zip, .tar.gz, .tar.xz) and OCI registry
artifacts (oci:// prefix). For OCI artifacts, the command pulls the artifact
from the registry and reads its embedded metadata.`,
	Args: cobra.ExactArgs(1),
	RunE: runMetadata,
}

func runMetadata(cmd *cobra.Command, args []string) error {
	artifactPath := args[0]

	// If the artifact is an OCI source, pull it first
	if strings.HasPrefix(artifactPath, "oci://") {
		return runMetadataOCI(cmd, artifactPath)
	}

	metadata, err := tofupress.ReadMetadataFromArtifact(artifactPath)
	if err != nil {
		return err
	}

	return writeMetadataOutput(cmd, metadata)
}

// runMetadataOCI pulls an OCI artifact and reads its embedded metadata.
func runMetadataOCI(cmd *cobra.Command, sourceURL string) error {
	// Pull through an owned fetcher so warnings follow Cobra's configured
	// error writer rather than bypassing the command on global stderr.
	fetcher := tofupress.NewFetcher(tofupress.WithWarningWriter(cmd.ErrOrStderr()))
	workDir, _, cleanup, err := resolveSourceWithFetcher(cmd.Context(), sourceURL, fetcher)
	if err != nil {
		return fmt.Errorf("failed to pull OCI artifact: %w", err)
	}
	defer cleanup()

	// Read meta.json from the extracted artifact directory
	metadata, err := tofupress.ReadMetadataFromDir(workDir)
	if err != nil {
		return fmt.Errorf("failed to read metadata from OCI artifact: %w", err)
	}

	return writeMetadataOutput(cmd, metadata)
}

// writeMetadataOutput marshals metadata as indented JSON and writes it to the command output.
func writeMetadataOutput(cmd *cobra.Command, metadata *tofupress.ArtifactMetadata) error {
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode metadata: %w", err)
	}
	data = append(data, '\n')
	_, err = cmd.OutOrStdout().Write(data)
	return err
}
