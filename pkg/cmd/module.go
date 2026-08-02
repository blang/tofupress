package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/blang/tofupress/pkg/tofupress"
)

var moduleCmd = &cobra.Command{
	Use:   "module <subject> [output]",
	Short: "Press one module: resolve deps and create a self-contained archive",
	Long: `Presses ONE module: resolves all module dependencies and creates a self-contained archive,
pivoting the entry module to the archive root (ADR-0002).

The subject must contain at least one .tf/.tofu file — a no-.tf subject is refused; use
` + "`tofupress tree`" + ` for a modules-only repository.

Supported formats:
  zip     - For OCI registry distribution (default)
  tar.gz  - For HTTP server distribution
  tar.xz  - For S3/object storage distribution

Example:
  tofupress module ./infra bundle.zip
  oras push --artifact-type=application/vnd.opentofu.modulepkg \
    registry.example.com/module:latest bundle.zip:archive/zip`,
	Args: cobra.ExactArgs(2),
	RunE: runPressModule,
}

func init() {
	moduleCmd.Flags().String("format", "auto", "Bundle format: auto, zip, tar.gz, tar.xz (auto detects from output file extension)")
	moduleCmd.Flags().Bool("oci-compliant", false, "Generate OCI-compliant bundle (no sourcetree metadata, inlined modules)")
	moduleCmd.Flags().Bool("strict-oci", true, "Strict OCI spec enforcement: reject artifacts with empty/non-matching artifactType (review item 10; --strict-oci=false = lenient with warning)")
	moduleCmd.Flags().Bool("json", false, "Emit the bundle metadata as JSON to stdout after success (review item 9)")
	moduleCmd.Flags().String("metadata-out", "", "Write bundle metadata JSON to a separate path")
	moduleCmd.Flags().String("strip", "optimistic", "Strip level: full, optimistic (default), aggressive. Legacy aliases: none=full, module-dir=optimistic, config-only/tf-only=aggressive (ADR-0001)")
	moduleCmd.Flags().String("vendor-dir", "_vendor", "Vendored modules directory name (remote dependencies are rooted here in the bundle)")
}

// runPressModule is the `tofupress module` adapter (ADR-0002): a single-module
// press that PIVOTS the entry to the archive root. It resolves the source,
// refuses a no-.tf subject, and hands off to the shared pressing pipeline.
//
//nolint:gocognit,gocyclo // CLI wiring naturally involves multiple configuration steps
func runPressModule(cmd *cobra.Command, args []string) error {
	source := args[0]
	outputPath := args[1]
	stdout := cmd.OutOrStdout()

	if err := validatePressFlags(cmd, outputPath); err != nil {
		return err
	}

	jsonOut, _ := cmd.Flags().GetBool("json")
	if !jsonOut {
		fmt.Fprintf(stdout, "Resolving modules in %s...\n", tofupress.RedactSourceAddress(source)) //nolint:errcheck // stdout writes are best-effort
	}

	fetcher := newCommandFetcher(cmd)
	workDir, packageRoot, cleanup, err := resolveSourceWithFetcher(cmd.Context(), source, fetcher)
	if err != nil {
		return err
	}
	defer cleanup()
	stopSig := installSignalCleanup(cleanup)
	defer stopSig()

	// `tofupress module` refuses a no-.tf subject (ADR-0002): a module press
	// requires at least one .tf/.tofu in the entry module; a modules-only repo is
	// the `tofupress tree` case. Fail fast before resolution rather than warn-
	// and-degenerate into an empty archive (the former `bundle` behaviour).
	tfFiles, tfErr := tofupress.FindTerraformFiles(workDir)
	if tfErr == nil && len(tfFiles) == 0 {
		return fmt.Errorf("subject %q contains no .tf or .tofu files; `tofupress module` presses a single module — use `tofupress tree` for a modules-only repository (ADR-0002)", tofupress.RedactSourceAddress(source))
	}

	resolver := newPressResolverWithFetcher(cmd, fetcher, packageRoot, workDir)
	return runPress(cmd, source, outputPath, workDir, resolver.Resolve, "module")
}
