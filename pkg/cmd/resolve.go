package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blang/tofupress/pkg/tofupress"
)

var resolveCmd = &cobra.Command{
	Use:   "resolve <directory>",
	Short: "Resolve all modules in a Terraform/OpenTofu configuration",
	Long:  `Scans the given directory for Terraform/OpenTofu files and resolves all module dependencies.`,
	Args:  cobra.ExactArgs(1),
	RunE:  runResolve,
}

func init() {
	resolveCmd.Flags().Bool("json", false, "Output in JSON format")
	resolveCmd.Flags().String("vendor-dir", "_vendor", "Vendored modules directory name (remote dependencies are rooted here during resolution)")
}

//nolint:gocognit // JSON and text output branching is straightforward
func runResolve(cmd *cobra.Command, args []string) error {
	dir := args[0]
	stdout := cmd.OutOrStdout()

	// Resolve source (local or remote) to a working directory
	workDir, packageRoot, cleanup, err := resolveSource(cmd.Context(), dir)
	if err != nil {
		return err
	}
	defer cleanup()

	// Resolve modules in temp directory
	resolver := tofupress.NewResolver()
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
			fmt.Fprintf(cmd.ErrOrStderr(), "  Resolving module: %s\n", event.ModuleName) //nolint:errcheck // stderr writes are best-effort
		case "downloading":
			fmt.Fprintf(cmd.ErrOrStderr(), "  ⬇ Downloading: %s from %s\n", event.ModuleName, event.Source) //nolint:errcheck // stderr writes are best-effort
		case "downloaded":
			fmt.Fprintf(cmd.ErrOrStderr(), "  ✓ Downloaded: %s\n", event.ModuleName) //nolint:errcheck // stderr writes are best-effort
		case "warning":
			fmt.Fprintf(cmd.ErrOrStderr(), "  ⚠ Warning: %s\n", event.Source) //nolint:errcheck // stderr writes are best-effort
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

	// Check if JSON output requested
	jsonOutput, _ := cmd.Flags().GetBool("json")

	if jsonOutput {
		return outputJSON(stdout, tree)
	}

	return outputText(stdout, tree)
}

//nolint:gocognit // JSON marshaling with cycle detection is complex but clear
func outputJSON(stdout io.Writer, tree *tofupress.ResolvedTree) error {
	// Output as JSON - use a cycle-safe representation with paths made relative
	// to avoid leaking ephemeral temp directory paths.
	type jsonModule struct {
		Key        string   `json:"key"`
		Name       string   `json:"name"`
		Source     string   `json:"source"`
		SourceType string   `json:"source_type"`
		InstallDir string   `json:"install_dir,omitempty"`
		Children   []string `json:"children"`
		IsLocal    bool     `json:"is_local"`
		IsRemote   bool     `json:"is_remote"`
	}

	type jsonPackage struct {
		PackageAddr string `json:"package_addr"`
		LocalDir    string `json:"local_dir,omitempty"`
	}

	rootDir := tree.Root.InstallDir
	modules := make([]jsonModule, 0, len(tree.AllModules))
	for _, mod := range tree.AllModules {
		childNames := make([]string, 0, len(mod.Children))
		for _, child := range mod.Children {
			childNames = append(childNames, child.Key)
		}
		modules = append(modules, jsonModule{
			Key:        mod.Key,
			Name:       mod.Name,
			Source:     mod.Source.PackageAddr,
			SourceType: mod.Source.Type.String(),
			IsLocal:    mod.IsLocal,
			IsRemote:   mod.IsRemote,
			InstallDir: tofupress.FormatInstallDir(mod.InstallDir, rootDir),
			Children:   childNames,
		})
	}

	packages := make([]jsonPackage, 0, len(tree.Packages))
	for _, pkg := range tree.Packages {
		packages = append(packages, jsonPackage{
			PackageAddr: pkg.PackageAddr,
			LocalDir:    tofupress.FormatInstallDir(pkg.LocalDir, rootDir),
		})
	}

	output := map[string]any{
		tofupress.SourceDisplayRoot: tree.Root.Key,
		"modules":                   modules,
		"packages":                  packages,
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(output)
}

//nolint:unparam // error return is part of the output function interface
func outputText(stdout io.Writer, tree *tofupress.ResolvedTree) error {
	// Output as human-readable text
	fmt.Fprintln(stdout, "Module tree:") //nolint:errcheck // stdout writes are best-effort
	fmt.Fprintln(stdout, "============") //nolint:errcheck // stdout writes are best-effort
	fmt.Fprintln(stdout)                 //nolint:errcheck // stdout writes are best-effort

	// Print tree structure
	printModuleTree(stdout, tree.Root, 0)

	fmt.Fprintln(stdout)                                             //nolint:errcheck // stdout writes are best-effort
	fmt.Fprintf(stdout, "Total modules: %d\n", len(tree.AllModules)) //nolint:errcheck // stdout writes are best-effort
	fmt.Fprintf(stdout, "Total packages: %d\n", len(tree.Packages))  //nolint:errcheck // stdout writes are best-effort

	return nil
}

func printModuleTree(w io.Writer, node *tofupress.ModuleNode, depth int) {
	var indent strings.Builder
	for range depth {
		indent.WriteString("  ")
	}

	// Print module name and source
	moduleType := tofupress.SourceDisplayLocal
	if node.IsRemote {
		moduleType = "remote"
	}

	fmt.Fprintf(w, "%s- %s (%s)\n", indent.String(), node.Name, moduleType) //nolint:errcheck // writer writes are best-effort
	if node.Source.PackageAddr != "" {
		fmt.Fprintf(w, "%s  Source: %s\n", indent.String(), node.Source.PackageAddr) //nolint:errcheck // writer writes are best-effort
	}

	// Print children
	for _, child := range node.Children {
		printModuleTree(w, child, depth+1)
	}
}
