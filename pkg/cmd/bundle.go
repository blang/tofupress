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
	bundleCmd.Flags().Bool("oci-compliant", false, "Generate OCI-compliant bundle (no sourcetree metadata, inlined modules)")
	bundleCmd.Flags().String("metadata-out", "", "Write bundle metadata JSON to a separate path")
}

//
//nolint:gocognit,gocyclo // CLI wiring naturally involves multiple configuration steps
func runBundle(cmd *cobra.Command, args []string) error {
	source := args[0]
	outputPath := args[1]
	stdout := cmd.OutOrStdout()

	// Ensure output directory exists
	outputDir := filepath.Dir(outputPath)
	if err := os.MkdirAll(outputDir, 0o755); err != nil { //nolint:gosec // G301: 0755 is standard for directories
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	fmt.Fprintf(stdout, "Resolving modules in %s...\n", source) //nolint:errcheck // stdout writes are best-effort

	// Resolve source (local or remote) to a working directory
	workDir, packageRoot, cleanup, err := resolveSource(cmd.Context(), source)
	if err != nil {
		return err
	}
	defer cleanup()

	// Resolve modules in temp directory
	resolver := tofupress.NewResolver()
	resolver.PackageRoot = packageRoot // Set package boundary for local path enforcement
	resolver.Progress = func(event *tofupress.ProgressEvent) {
		if event == nil {
			return
		}
		switch event.Type {
		case "resolving":
			fmt.Fprintf(os.Stderr, "  Resolving module: %s\n", event.ModuleName)
		case "downloading":
			fmt.Fprintf(os.Stderr, "  ⬇ Downloading: %s from %s\n", event.ModuleName, event.Source)
		case "downloaded":
			fmt.Fprintf(os.Stderr, "  ✓ Downloaded: %s\n", event.ModuleName)
		}
	}
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
		if !ok {
			return fmt.Errorf("could not infer bundle format from output path %q; pass --format=zip, --format=tar.gz, or --format=tar.xz", outputPath)
		}
		format = detected
	}

	bundler := tofupress.NewBundler(format)
	ociCompliant, _ := cmd.Flags().GetBool("oci-compliant")
	if ociCompliant {
		if format != tofupress.BundleFormatZIP {
			return fmt.Errorf("--oci-compliant requires zip format (got %s)", format)
		}
		bundler.OCICompliant = true
	}

	metadata, err := tofupress.BuildArtifactMetadata(tree, &tofupress.MetadataRequest{
		Build: tofupress.BuildInfo{
			Version: BuildVersion,
			Commit:  BuildCommit,
			Time:    BuildTime,
		},
		Command:    "bundle",
		Args:       []string{source, outputPath},
		Options:    tofupress.BundleOptions{Format: string(format), OCICompliant: ociCompliant, StripMode: "none"},
		RootSource: source,
		OutputPath: outputPath,
	})
	if err != nil {
		return fmt.Errorf("failed to build metadata: %w", err)
	}
	bundler.Metadata = metadata

	if bundleErr := bundler.Bundle(tree, outputPath); bundleErr != nil {
		return fmt.Errorf("failed to create bundle: %w", bundleErr)
	}

	// Get bundle size
	info, err := os.Stat(outputPath)
	if err != nil {
		return fmt.Errorf("failed to stat bundle: %w", err)
	}

	fmt.Fprintf(stdout, "Bundle created successfully: %s (%.2f MB)\n", outputPath, float64(info.Size())/(1024*1024)) //nolint:errcheck // stdout writes are best-effort

	metadataOut, _ := cmd.Flags().GetString("metadata-out")
	if metadataOut != "" {
		if err := tofupress.WriteMetadataFile(metadataOut, metadata); err != nil {
			return fmt.Errorf("failed to write metadata output: %w", err)
		}
	}

	fmt.Fprintf(stdout, "Module references: %d\n", metadata.Stats.ModuleReferences) //nolint:errcheck // stdout writes are best-effort
	fmt.Fprintf(stdout, "Unique packages: %d\n", metadata.Stats.UniquePackages)     //nolint:errcheck // stdout writes are best-effort
	fmt.Fprintf(stdout, "Source types: %v\n", metadata.Stats.SourceTypes)           //nolint:errcheck // stdout writes are best-effort
	fmt.Fprintf(stdout, "Original bytes: %d\n", metadata.Stats.OriginalBytes)       //nolint:errcheck // stdout writes are best-effort
	fmt.Fprintf(stdout, "Final bytes: %d\n", metadata.Stats.FinalBytes)             //nolint:errcheck // stdout writes are best-effort
	fmt.Fprintf(stdout, "Stripped bytes: %d\n", metadata.Stats.StrippedBytes)       //nolint:errcheck // stdout writes are best-effort
	fmt.Fprintf(stdout, "Metadata: %s\n", tofupress.MetadataFileName)               //nolint:errcheck // stdout writes are best-effort
	if metadataOut != "" {
		fmt.Fprintf(stdout, "Metadata: %s\n", metadataOut) //nolint:errcheck // stdout writes are best-effort
	}

	return nil
}
