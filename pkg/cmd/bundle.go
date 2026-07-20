package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	bundleCmd.Flags().Bool("strict-oci", true, "Strict OCI spec enforcement: reject artifacts with empty/non-matching artifactType (review item 10; --strict-oci=false = lenient with warning)")
	bundleCmd.Flags().Bool("json", false, "Emit the bundle metadata as JSON to stdout after success (review item 9)")
	bundleCmd.Flags().String("metadata-out", "", "Write bundle metadata JSON to a separate path")
	bundleCmd.Flags().String("strip", "optimistic", "Strip level: full, optimistic (default), aggressive. Legacy aliases: none=full, module-dir=optimistic, config-only/tf-only=aggressive (ADR-0001)")
	bundleCmd.Flags().String("vendor-dir", "_vendor", "Vendored modules directory name (remote dependencies are rooted here in the bundle)")
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

	jsonOut, _ := cmd.Flags().GetBool("json")
	if !jsonOut {
		fmt.Fprintf(stdout, "Resolving modules in %s...\n", source) //nolint:errcheck // stdout writes are best-effort
	}

	// Resolve source (local or remote) to a working directory
	workDir, packageRoot, cleanup, err := resolveSource(cmd.Context(), source)
	if err != nil {
		return err
	}
	defer cleanup()
	stopSig := installSignalCleanup(cleanup)
	defer stopSig()

	// Resolve modules in temp directory
	// Build the fetcher with the OCI strictness flag (review item 10) and inject
	// it via the options-pattern affordance added in item 5's API companion.
	strictOCI, _ := cmd.Flags().GetBool("strict-oci")
	fetcher := tofupress.NewFetcher(tofupress.WithStrictOCI(strictOCI))
	resolver := tofupress.NewResolver(tofupress.WithFetcher(fetcher))
	resolver.PackageRoot = packageRoot // Set package boundary for local path enforcement
	resolver.RootDir = workDir         // Set root dir for user-friendly error message paths
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

	// Check for empty root module (no .tf/.tofu files)
	tfFiles, err := tofupress.FindTerraformFiles(workDir)
	if err == nil && len(tfFiles) == 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: root module contains no .tf or .tofu files\n") //nolint:errcheck // stderr writes are best-effort
	}
	if !jsonOut {
		fmt.Fprintf(stdout, "Found %d modules and %d packages\n", len(tree.AllModules), len(tree.Packages)) //nolint:errcheck // stdout writes are best-effort
		fmt.Fprintf(stdout, "Creating bundle at %s...\n", outputPath)                                       //nolint:errcheck // stdout writes are best-effort
	}

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
			hint := ""
			if strings.HasSuffix(outputPath, "/") || strings.HasSuffix(outputPath, string(filepath.Separator)) {
				hint = " (the output path looks like a directory; tofupress writes an archive file, not a directory -- pass a file path ending in .zip/.tar.gz/.tar.xz)"
			}
			return fmt.Errorf("could not infer bundle format from output path %q; pass --format=zip, --format=tar.gz, or --format=tar.xz%s", outputPath, hint)
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
	stripPlan, err := tofupress.PlanStripping(cmd.Context(), tree, stripMode)
	if err != nil {
		return fmt.Errorf("failed to plan stripping: %w", err)
	}
	bundler.StripPlan = stripPlan

	// Plan and apply sourcetree identity (archive-only; OCI-compliant inlines modules)
	var sourcetreePlan *tofupress.SourcetreeIdentityPlan
	if !ociCompliant {
		sourcetreePlan, err = tofupress.BuildSourcetreeIdentityPlan(cmd.Context(), tree, stripPlan)
		if err != nil {
			return fmt.Errorf("failed to plan sourcetree identity: %w", err)
		}
		if applyErr := tofupress.ApplySourcetreeIdentityPlan(cmd.Context(), tree, sourcetreePlan); applyErr != nil {
			return fmt.Errorf("failed to apply sourcetree identity: %w", applyErr)
		}
		// Re-plan stripping after identity application since package directories changed
		stripPlan, err = tofupress.PlanStripping(cmd.Context(), tree, stripMode)
		if err != nil {
			return fmt.Errorf("failed to plan final stripping: %w", err)
		}
		bundler.StripPlan = stripPlan
	}

	// Print strip warnings from the FINAL applied strip plan (post-identity
	// re-plan for non-OCI builds), so the warnings a user sees match the
	// package layout actually archived and embedded in metadata. Printing the
	// pre-identity plan's warnings would surface stale excluded-dir hints
	// when identity moved the package roots.
	for _, warning := range stripPlan.Warnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %s\n", warning.Message) //nolint:errcheck // stderr writes are best-effort
	}

	metadata, err := tofupress.BuildArtifactMetadata(tree, &tofupress.MetadataRequest{
		Build:          EffectiveBuildInfo(),
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

	if bundleErr := bundler.Bundle(cmd.Context(), tree, outputPath); bundleErr != nil {
		return fmt.Errorf("failed to create bundle: %w", bundleErr)
	}

	// Get bundle size
	info, err := os.Stat(outputPath)
	if err != nil {
		return fmt.Errorf("failed to stat bundle: %w", err)
	}

	if !jsonOut {
		fmt.Fprintf(stdout, "Bundle created successfully: %s (%.2f MB)\n", outputPath, float64(info.Size())/(1024*1024)) //nolint:errcheck // stdout writes are best-effort
	}

	metadataOut, _ := cmd.Flags().GetString("metadata-out")
	if metadataOut != "" {
		if err := tofupress.WriteMetadataFile(metadataOut, metadata); err != nil {
			return fmt.Errorf("failed to write metadata output: %w", err)
		}
	}

	// --json: emit the metadata JSON to stdout for scriptable CI (review item 9).
	// Mirrors --metadata-out but to stdout, so CI need not parse human text or
	// juggle a temp file. jsonOut was read above.
	if jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(metadata); err != nil {
			return fmt.Errorf("failed to write metadata JSON: %w", err)
		}
		return nil
	}

	fmt.Fprintf(stdout, "Module references: %d\n", metadata.Stats.ModuleReferences)                    //nolint:errcheck // stdout writes are best-effort
	fmt.Fprintf(stdout, "Unique packages: %d\n", metadata.Stats.UniquePackages)                        //nolint:errcheck // stdout writes are best-effort
	fmt.Fprintf(stdout, "Deduplicated packages: %d\n", metadata.Stats.DeduplicatedPackages)            //nolint:errcheck // stdout writes are best-effort
	fmt.Fprintf(stdout, "Source types: %s\n", tofupress.FormatSourceTypes(metadata.Stats.SourceTypes)) //nolint:errcheck // stdout writes are best-effort
	fmt.Fprintf(stdout, "Original bytes: %d\n", metadata.Stats.OriginalBytes)                          //nolint:errcheck // stdout writes are best-effort
	fmt.Fprintf(stdout, "Final bytes: %d\n", metadata.Stats.FinalBytes)                                //nolint:errcheck // stdout writes are best-effort
	fmt.Fprintf(stdout, "Stripped bytes: %d\n", metadata.Stats.StrippedBytes)                          //nolint:errcheck // stdout writes are best-effort
	fmt.Fprintf(stdout, "Metadata: %s\n", tofupress.MetadataFileName)                                  //nolint:errcheck // stdout writes are best-effort
	if metadataOut != "" {
		fmt.Fprintf(stdout, "Metadata: %s\n", metadataOut) //nolint:errcheck // stdout writes are best-effort
	}

	return nil
}
