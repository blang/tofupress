package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blang/tofupress/pkg/tofupress"
)

// newPressResolver builds the Resolver shared by `module` and `tree`, wired with
// the strict-OCI fetcher, the progress callback, the package boundary, and the
// display-path root dir. The two commands differ only in resolveFn (Resolve vs
// ResolveTree) and in what packageRoot they pass: `module` uses the // -expanded
// package root (so ../ from the entry resolves inside the package); `tree` uses
// the subject itself (the layout preserved as-is, boundary = subject). This is
// the ADR-0002 seam: two thin adapters over one shared pressing pipeline.
func newCommandFetcher(cmd *cobra.Command) *tofupress.Fetcher {
	strictOCI, _ := cmd.Flags().GetBool("strict-oci")
	return tofupress.NewFetcher(
		tofupress.WithStrictOCI(strictOCI),
		tofupress.WithWarningWriter(cmd.ErrOrStderr()),
	)
}

func newPressResolver(cmd *cobra.Command, packageRoot, workDir string) *tofupress.Resolver {
	return newPressResolverWithFetcher(cmd, newCommandFetcher(cmd), packageRoot, workDir)
}

func newPressResolverWithFetcher(
	cmd *cobra.Command,
	fetcher *tofupress.Fetcher,
	packageRoot, workDir string,
) *tofupress.Resolver {
	resolver := tofupress.NewResolver(tofupress.WithFetcher(fetcher))
	resolver.PackageRoot = packageRoot // package boundary for local-path enforcement
	resolver.RootDir = workDir         // root dir for user-friendly error message paths
	vendorDir, _ := cmd.Flags().GetString("vendor-dir")
	if vendorDir != "" {
		resolver.VendorDir = vendorDir
	}
	stderr := cmd.ErrOrStderr()
	resolver.Progress = func(event *tofupress.ProgressEvent) {
		if event == nil {
			return
		}
		switch event.Type {
		case "resolving":
			fmt.Fprintf(stderr, "  Resolving module: %s\n", event.ModuleName) //nolint:errcheck // stderr writes are best-effort
		case "downloading":
			fmt.Fprintf(stderr, "  ⬇ Downloading: %s from %s\n", event.ModuleName, event.Source) //nolint:errcheck // stderr writes are best-effort
		case "downloaded":
			fmt.Fprintf(stderr, "  ✓ Downloaded: %s\n", event.ModuleName) //nolint:errcheck // stderr writes are best-effort
		case "warning":
			fmt.Fprintf(stderr, "  ⚠ Warning: %s\n", event.Source) //nolint:errcheck // stderr writes are best-effort
		}
	}
	return resolver
}

func validatePressFlags(cmd *cobra.Command, outputPath string) error {
	format, err := resolvePressFormat(cmd, outputPath)
	if err != nil {
		return err
	}
	ociCompliant, _ := cmd.Flags().GetBool("oci-compliant")
	if ociCompliant && format != tofupress.BundleFormatZIP {
		return fmt.Errorf("--oci-compliant requires zip format (got %s)", format)
	}
	stripInput, _ := cmd.Flags().GetString("strip")
	if _, err := tofupress.ParseStripMode(stripInput); err != nil {
		return err
	}
	return validateVendorDirFlag(cmd)
}

func resolvePressFormat(cmd *cobra.Command, outputPath string) (tofupress.BundleFormat, error) {
	formatInput, _ := cmd.Flags().GetString("format")
	format, err := tofupress.ParseBundleFormat(formatInput)
	if err != nil {
		return "", err
	}
	if format != tofupress.BundleFormatAuto {
		return format, nil
	}
	detected, ok := tofupress.DetectFormatFromPath(outputPath)
	if ok {
		return detected, nil
	}
	hint := ""
	if strings.HasSuffix(outputPath, "/") || strings.HasSuffix(outputPath, string(filepath.Separator)) {
		hint = " (the output path looks like a directory; tofupress writes an archive file, not a directory -- pass a file path ending in .zip/.tar.gz/.tar.xz)"
	}
	return "", fmt.Errorf("could not infer bundle format from output path %q; pass --format=zip, --format=tar.gz, or --format=tar.xz%s", outputPath, hint)
}

func validateVendorDirFlag(cmd *cobra.Command) error {
	if cmd.Flags().Lookup("vendor-dir") == nil {
		return nil
	}
	vendorDir, err := cmd.Flags().GetString("vendor-dir")
	if err != nil {
		return fmt.Errorf("read --vendor-dir: %w", err)
	}
	return tofupress.ValidateVendorDir(vendorDir)
}

// resolveFunc is the shared signature of Resolver.Resolve and Resolver.ResolveTree.
type resolveFunc func(context.Context, string) (*tofupress.ResolvedTree, error)

// runPress is the shared pressing pipeline behind `tofupress module` and
// `tofupress tree` (ADR-0002). The adapter supplies the bound resolveFn (a
// method value of a configured Resolver — single-entry Resolve or multi-entry
// ResolveTree), the workDir to resolve, and the command name (for metadata).
// Everything from the resolution call through format detection, strip planning,
// sourcetree identity, metadata, bundle, and stdout/json output is identical
// across the two commands — the command shape (pivot vs unpivoted, single entry
// vs many) is encoded entirely in resolveFn and the resolver's package boundary
// (set by the adapter before binding resolveFn).
//
//nolint:gocognit,gocyclo // CLI wiring naturally involves multiple configuration steps
func runPress(cmd *cobra.Command, source, outputPath, workDir string, resolveFn resolveFunc, commandName string) error {
	stdout := cmd.OutOrStdout()
	jsonOut, _ := cmd.Flags().GetBool("json")

	// Ensure output directory exists (the archive is a single file, not a dir).
	outputDir := filepath.Dir(outputPath)
	if err := os.MkdirAll(outputDir, 0o755); err != nil { //nolint:gosec // G301: 0755 is standard for directories
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	tree, err := resolveFn(cmd.Context(), workDir)
	if err != nil {
		return fmt.Errorf("failed to resolve modules: %w", err)
	}
	if !jsonOut {
		fmt.Fprintf(stdout, "Found %d modules and %d packages\n", len(tree.AllModules), len(tree.Packages)) //nolint:errcheck // stdout writes are best-effort
		fmt.Fprintf(stdout, "Creating bundle at %s...\n", outputPath)                                       //nolint:errcheck // stdout writes are best-effort
	}

	// Create bundle. Adapters already run this validation before source
	// acquisition; resolve it again here to keep runPress safe as a direct seam.
	format, err := resolvePressFormat(cmd, outputPath)
	if err != nil {
		return err
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
		Command:        commandName,
		Args:           []string{source, outputPath},
		Options:        tofupress.BundleOptions{Format: string(format), OCICompliant: ociCompliant, StripMode: string(stripMode), StripModeInput: stripStr},
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
