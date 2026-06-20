package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/blang/tofupress/pkg/tofupress"
)

var bundleCmd = &cobra.Command{
	Use:   "bundle <directory> <output>",
	Short: "Resolve modules and create a bundle",
	Long: `Resolves all module dependencies and creates a self-contained bundle.

Supported formats:
  zip     - For OCI registry distribution (default)
  tar.gz  - For HTTP server distribution
  tar.xz  - For S3/object storage distribution

Example:
  tofupress bundle ./infra bundle.zip
  oras push --artifact-type=application/vnd.opentofu.modulepkg \
    registry.example.com/module:latest bundle.zip:archive/zip`,
	Args: cobra.ExactArgs(2),
	RunE: runBundle,
}

func init() {
	bundleCmd.Flags().String("format", "auto", "Bundle format: auto, zip, tar.gz, tar.xz (auto detects from output file extension)")
}

func runBundle(cmd *cobra.Command, args []string) error {
	source := args[0]
	outputPath := args[1]
	stdout := cmd.OutOrStdout()

	// Ensure output directory exists
	outputDir := filepath.Dir(outputPath)
	if outputDir != "" {
		if err := os.MkdirAll(outputDir, 0o755); err != nil { //nolint:gosec // G301: 0755 is standard for directories
			return fmt.Errorf("failed to create output directory: %w", err)
		}
	}

	fmt.Fprintf(stdout, "Resolving modules in %s...\n", source) //nolint:errcheck // stdout writes are best-effort

	// Resolve source (local or remote) to a working directory
	workDir, cleanup, err := resolveSource(cmd.Context(), source)
	if err != nil {
		return err
	}
	defer cleanup()

	// Resolve modules in temp directory
	resolver := tofupress.NewResolver()
	tree, err := resolver.Resolve(cmd.Context(), workDir)
	if err != nil {
		return fmt.Errorf("failed to resolve modules: %w", err)
	}

	fmt.Fprintf(stdout, "Found %d modules and %d packages\n", len(tree.AllModules), len(tree.Packages)) //nolint:errcheck // stdout writes are best-effort
	fmt.Fprintf(stdout, "Creating bundle at %s...\n", outputPath)                                       //nolint:errcheck // stdout writes are best-effort

	// Create bundle
	formatStr, _ := cmd.Flags().GetString("format")
	format, err := tofupress.ParseBundleFormat(formatStr)
	if err != nil {
		return err
	}

	// Auto-detect format from output file extension if not explicitly set
	if format == tofupress.BundleFormatAuto {
		detected, ok := tofupress.DetectFormatFromPath(outputPath)
		if ok {
			format = detected
		} else {
			fmt.Fprintf(os.Stderr, "Warning: could not infer format from output file '%s', defaulting to zip\n", outputPath) //nolint:errcheck // stderr is best-effort
			format = tofupress.BundleFormatZIP
		}
	}

	bundler := tofupress.NewBundler(format)
	if bundleErr := bundler.Bundle(tree, outputPath); bundleErr != nil {
		return fmt.Errorf("failed to create bundle: %w", bundleErr)
	}

	// Get bundle size
	info, err := os.Stat(outputPath)
	if err != nil {
		return fmt.Errorf("failed to stat bundle: %w", err)
	}

	fmt.Fprintf(stdout, "Bundle created successfully: %s (%.2f MB)\n", outputPath, float64(info.Size())/(1024*1024)) //nolint:errcheck // stdout writes are best-effort

	return nil
}
