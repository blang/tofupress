package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/blang/tofupress/pkg/tofupress"
)

var metadataCmd = &cobra.Command{
	Use:   "metadata <artifact>",
	Short: "Print metadata embedded in a TofuPress artifact",
	Args:  cobra.ExactArgs(1),
	RunE:  runMetadata,
}

func runMetadata(cmd *cobra.Command, args []string) error {
	metadata, err := tofupress.ReadMetadataFromArtifact(args[0])
	if err != nil {
		return err
	}

	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode metadata: %w", err)
	}
	data = append(data, '\n')
	_, err = cmd.OutOrStdout().Write(data)
	return err
}
