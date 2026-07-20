package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var treeCmd = &cobra.Command{
	Use:   "tree <subject> [output]",
	Short: "Press a tree of modules: resolve every .tf-dir under the subject, unpivoted",
	Long: `Presses the subject AS-IS, unpivoted: every .tf/.tofu-bearing directory under the
subject is an anchor kept at its staged path, so consumers address any of them via "//".
Local ../ cross-references between modules are preserved verbatim (layout unchanged);
only remote/registry sources are rewritten and vendored. The package boundary is the
subject itself (ADR-0002).

A subject with no .tf/.tofu directories anywhere is refused; use ` + "`tofupress module`" + ` to
press a single module.

Supported formats:
  zip     - For OCI registry distribution (default)
  tar.gz  - For HTTP server distribution
  tar.xz  - For S3/object storage distribution

Example:
  tofupress tree ./myrepo repo.zip
  tofupress tree github.com/owner/modules-repo//modules modules.zip`,
	Args: cobra.ExactArgs(2),
	RunE: runPressTree,
}

func init() {
	treeCmd.Flags().String("format", "auto", "Bundle format: auto, zip, tar.gz, tar.xz (auto detects from output file extension)")
	treeCmd.Flags().Bool("oci-compliant", false, "Generate OCI-compliant bundle (no sourcetree metadata, inlined modules)")
	treeCmd.Flags().Bool("strict-oci", true, "Strict OCI spec enforcement: reject artifacts with empty/non-matching artifactType (review item 10; --strict-oci=false = lenient with warning)")
	treeCmd.Flags().Bool("json", false, "Emit the bundle metadata as JSON to stdout after success (review item 9)")
	treeCmd.Flags().String("metadata-out", "", "Write bundle metadata JSON to a separate path")
	treeCmd.Flags().String("strip", "optimistic", "Strip level: full, optimistic (default), aggressive. Legacy aliases: none=full, module-dir=optimistic, config-only/tf-only=aggressive (ADR-0001)")
	treeCmd.Flags().String("vendor-dir", "_vendor", "Vendored modules directory name (remote dependencies are rooted here in the bundle)")
}

// runPressTree is the `tofupress tree` adapter (ADR-0002): a multi-entry,
// unpivoted press. The package boundary is the subject itself (workDir), NOT the
// //-expanded package root — `tree` presses the layout as-is within the
// subject, so a local ref escaping the subject is a boundary error. The
// no-.tf-dirs-anywhere refusal lives inside Resolver.ResolveTree (so library
// callers get it too); the adapter only wires the resolver and hands off to the
// shared pressing pipeline.
//
//nolint:gocognit,gocyclo // CLI wiring naturally involves multiple configuration steps
func runPressTree(cmd *cobra.Command, args []string) error {
	source := args[0]
	outputPath := args[1]
	stdout := cmd.OutOrStdout()

	jsonOut, _ := cmd.Flags().GetBool("json")
	if !jsonOut {
		fmt.Fprintf(stdout, "Pressing tree at %s...\n", source) //nolint:errcheck // stdout writes are best-effort
	}

	// resolveSource fetches the subject (local copy or remote download) into a
	// temp workDir. For a //subdir input it returns packageRoot = the enclosing
	// package; `tree` overrides the boundary to workDir (the subject), so refs
	// escaping the subject are flagged rather than resolved against the enclosing
	// package — `tree` presses the subject as-is within its own boundary.
	workDir, _, cleanup, err := resolveSource(cmd.Context(), source)
	if err != nil {
		return err
	}
	defer cleanup()
	stopSig := installSignalCleanup(cleanup)
	defer stopSig()

	resolver := newPressResolver(cmd, workDir, workDir)
	return runPress(cmd, source, outputPath, workDir, resolver.ResolveTree, "tree")
}
