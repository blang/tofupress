package tofupress

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// RunCLI runs the CLI with the given arguments and output writers.
func RunCLI(args []string, stdout, stderr io.Writer) error {
	rootCmd := &cobra.Command{
		Use:   "tofupress",
		Short: "Terraform/OpenTofu module resolver and bundler",
		Long: `TofuPress resolves all module dependencies in a Terraform/OpenTofu
configuration and bundles them into a self-contained artifact.`,
	}

	rootCmd.SetOut(stdout)
	rootCmd.SetErr(stderr)
	rootCmd.SetArgs(args)

	// Add resolve command
	resolveCmd := &cobra.Command{
		Use:   "resolve <directory>",
		Short: "Resolve all modules in a Terraform/OpenTofu configuration",
		Long:  `Scans the given directory for Terraform/OpenTofu files and resolves all module dependencies.`,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runResolve(cmd, args, stdout)
		},
	}

	// Add JSON flag to resolve command
	resolveCmd.Flags().Bool("json", false, "Output in JSON format")

	// Add bundle command
	bundleCmd := &cobra.Command{
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
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBundle(cmd, args, stdout)
		},
	}
	bundleCmd.Flags().String("format", "zip", "Bundle format: zip, tar.gz, tar.xz")

	rootCmd.AddCommand(resolveCmd)
	rootCmd.AddCommand(bundleCmd)

	return rootCmd.Execute()
}

//nolint:unparam // stderr is part of the standard handler signature
func runResolve(cmd *cobra.Command, args []string, stdout io.Writer) error {
	dir := args[0]

	// Validate directory exists
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return fmt.Errorf("directory does not exist: %s", dir)
	}

	// Resolve modules
	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), dir)
	if err != nil {
		return fmt.Errorf("failed to resolve modules: %w", err)
	}

	// Check if JSON output requested
	jsonOutput, _ := cmd.Flags().GetBool("json")

	if jsonOutput {
		// Output as JSON - use a cycle-safe representation
		type jsonModule struct {
			Key        string   `json:"key"`
			Name       string   `json:"name"`
			Source     string   `json:"source"`
			InstallDir string   `json:"install_dir"`
			Children   []string `json:"children"`
			IsLocal    bool     `json:"is_local"`
			IsRemote   bool     `json:"is_remote"`
		}

		type jsonPackage struct {
			PackageAddr string `json:"package_addr"`
			LocalDir    string `json:"local_dir"`
		}

		modules := make([]jsonModule, 0, len(tree.AllModules))
		for _, mod := range tree.AllModules {
			childNames := make([]string, 0, len(mod.Children))
			for _, child := range mod.Children {
				childNames = append(childNames, child.Key)
			}
			modules = append(modules, jsonModule{
				Key:        mod.Key,
				Name:       mod.Name,
				Source:     mod.Source.Raw,
				IsLocal:    mod.IsLocal,
				IsRemote:   mod.IsRemote,
				InstallDir: mod.InstallDir,
				Children:   childNames,
			})
		}

		packages := make([]jsonPackage, 0, len(tree.Packages))
		for _, pkg := range tree.Packages {
			packages = append(packages, jsonPackage{
				PackageAddr: pkg.PackageAddr,
				LocalDir:    pkg.LocalDir,
			})
		}

		output := map[string]any{
			"root":     tree.Root.Key, //nolint:goconst // JSON key
			"modules":  modules,
			"packages": packages,
		}
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(output)
	}

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

func printModuleTree(w io.Writer, node *ModuleNode, depth int) {
	var indent strings.Builder
	for range depth {
		indent.WriteString("  ")
	}

	// Print module name and source
	moduleType := "local" //nolint:goconst // short descriptive string, not a shared constant
	if node.IsRemote {
		moduleType = "remote"
	}

	fmt.Fprintf(w, "%s- %s (%s)\n", indent.String(), node.Name, moduleType) //nolint:errcheck // writer writes are best-effort
	if node.Source.PackageAddr != "" {
		fmt.Fprintf(w, "%s  Source: %s\n", indent.String(), node.Source.PackageAddr) //nolint:errcheck // writer writes are best-effort
	}
	if node.InstallDir != "" {
		fmt.Fprintf(w, "%s  Path: %s\n", indent.String(), node.InstallDir) //nolint:errcheck // writer writes are best-effort
	}

	// Print children
	for _, child := range node.Children {
		printModuleTree(w, child, depth+1)
	}
}

func runBundle(cmd *cobra.Command, args []string, stdout io.Writer) error {
	dir := args[0]
	outputPath := args[1]

	// Validate directory exists
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return fmt.Errorf("directory does not exist: %s", dir)
	}

	// Ensure output directory exists
	outputDir := filepath.Dir(outputPath)
	if outputDir != "" {
		if err := os.MkdirAll(outputDir, 0o755); err != nil { //nolint:gosec // G301: 0755 is standard for directories
			return fmt.Errorf("failed to create output directory: %w", err)
		}
	}

	fmt.Fprintf(stdout, "Resolving modules in %s...\n", dir) //nolint:errcheck // stdout writes are best-effort

	// Resolve modules
	resolver := NewResolver()
	tree, err := resolver.Resolve(context.Background(), dir)
	if err != nil {
		return fmt.Errorf("failed to resolve modules: %w", err)
	}

	fmt.Fprintf(stdout, "Found %d modules and %d packages\n", len(tree.AllModules), len(tree.Packages)) //nolint:errcheck // stdout writes are best-effort
	fmt.Fprintf(stdout, "Creating bundle at %s...\n", outputPath)                                       //nolint:errcheck // stdout writes are best-effort

	// Create bundle
	formatStr, _ := cmd.Flags().GetString("format")
	format, err := ParseBundleFormat(formatStr)
	if err != nil {
		return err
	}
	bundler := NewBundler(format)
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
