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
	bundleCmd.Flags().String("strip", "module-dir", "Strip mode: none, module-dir (safe default), config-only, or tf-only (alias for config-only)")
	bundleCmd.Flags().String("vendor-dir", "sourcetree", "Vendored modules directory name")
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
	vendorDir, _ := cmd.Flags().GetString("vendor-dir")
	if vendorDir != "" {
		resolver.VendorDir = vendorDir
	}
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
		case "warning":
			fmt.Fprintf(os.Stderr, "  ⚠ Warning: %s\n", event.Source)
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

	// Parse and plan stripping
	stripStr, _ := cmd.Flags().GetString("strip")
	stripMode, err := tofupress.ParseStripMode(stripStr)
	if err != nil {
		return err
	}
	stripPlan, err := tofupress.PlanStripping(tree, stripMode)
	if err != nil {
		return fmt.Errorf("failed to plan stripping: %w", err)
	}
	bundler.StripPlan = stripPlan

	// Print strip warnings
	for _, warning := range stripPlan.Warnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %s\n", warning.Message) //nolint:errcheck // stderr writes are best-effort
	}

	// Plan and apply sourcetree identity (archive-only; OCI-compliant inlines modules)
	var sourcetreePlan *tofupress.SourcetreeIdentityPlan
	if !ociCompliant {
		sourcetreePlan, err = tofupress.BuildSourcetreeIdentityPlan(tree, stripPlan)
		if err != nil {
			return fmt.Errorf("failed to plan sourcetree identity: %w", err)
		}
		if applyErr := tofupress.ApplySourcetreeIdentityPlan(tree, sourcetreePlan); applyErr != nil {
			return fmt.Errorf("failed to apply sourcetree identity: %w", applyErr)
		}
		// Re-plan stripping after identity application since package directories changed
		stripPlan, err = tofupress.PlanStripping(tree, stripMode)
		if err != nil {
			return fmt.Errorf("failed to plan final stripping: %w", err)
		}
		bundler.StripPlan = stripPlan
	}

	metadata, err := tofupress.BuildArtifactMetadata(tree, &tofupress.MetadataRequest{
		Build: tofupress.BuildInfo{
			Version: BuildVersion,
			Commit:  BuildCommit,
			Time:    BuildTime,
		},
		Command:        "bundle",
		Args:           []string{source, outputPath},
		Options:        tofupress.BundleOptions{Format: string(format), OCICompliant: ociCompliant, StripMode: string(stripMode)},
		RootSource:     source,
		OutputPath:     outputPath,
		StripPlan:      stripPlan,
		SourcetreePlan: sourcetreePlan,
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

	fmt.Fprintf(stdout, "Module references: %d\n", metadata.Stats.ModuleReferences)         //nolint:errcheck // stdout writes are best-effort
	fmt.Fprintf(stdout, "Unique packages: %d\n", metadata.Stats.UniquePackages)             //nolint:errcheck // stdout writes are best-effort
	fmt.Fprintf(stdout, "Deduplicated packages: %d\n", metadata.Stats.DeduplicatedPackages) //nolint:errcheck // stdout writes are best-effort
	fmt.Fprintf(stdout, "Source types: %v\n", metadata.Stats.SourceTypes)                   //nolint:errcheck // stdout writes are best-effort
	fmt.Fprintf(stdout, "Original bytes: %d\n", metadata.Stats.OriginalBytes)               //nolint:errcheck // stdout writes are best-effort
	fmt.Fprintf(stdout, "Final bytes: %d\n", metadata.Stats.FinalBytes)                     //nolint:errcheck // stdout writes are best-effort
	fmt.Fprintf(stdout, "Stripped bytes: %d\n", metadata.Stats.StrippedBytes)               //nolint:errcheck // stdout writes are best-effort
	fmt.Fprintf(stdout, "Metadata: %s\n", tofupress.MetadataFileName)                       //nolint:errcheck // stdout writes are best-effort
	if metadataOut != "" {
		fmt.Fprintf(stdout, "Metadata: %s\n", metadataOut) //nolint:errcheck // stdout writes are best-effort
	}

	return nil
}
