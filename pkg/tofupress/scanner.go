package tofupress

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/zclconf/go-cty/cty"
)

// ModuleBlock represents a Terraform module block extracted from an HCL file.
type ModuleBlock struct {
	Name          string
	Source        string
	Version       string
	MissingSource bool // true when the module block has no source attribute
}

// String returns a string representation of the module block.
func (m ModuleBlock) String() string {
	return fmt.Sprintf("ModuleBlock{Name: %q, Source: %q}", m.Name, m.Source)
}

// FindTerraformFiles finds Terraform/OpenTofu configuration files in the given directory.
// It skips hidden files and directories, only returns files in the immediate directory,
// and applies OpenTofu priority: if both name.tf and name.tofu exist, name.tofu wins.
func FindTerraformFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read directory %s: %w", dir, err)
	}

	byBase := make(map[string]string)
	for _, entry := range entries {
		name := entry.Name()

		if entry.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}

		ext := filepath.Ext(name)
		if ext != ".tf" && ext != ".tofu" {
			continue
		}

		base := strings.TrimSuffix(name, ext)
		if ext == ".tofu" || byBase[base] == "" {
			byBase[base] = filepath.Join(dir, name)
		}
	}

	bases := make([]string, 0, len(byBase))
	for base := range byBase {
		bases = append(bases, base)
	}
	sort.Strings(bases)

	files := make([]string, 0, len(bases))
	for _, base := range bases {
		files = append(files, byBase[base])
	}

	return files, nil
}

// ExtractModuleBlocks parses an HCL file and extracts all module blocks.
// Returns a slice of ModuleBlock structs containing the module name and source.
// Modules with variable sources (non-string literals) are skipped.
//
//nolint:gocognit // HCL parsing with multiple conditional paths is inherently complex
func ExtractModuleBlocks(filePath string) ([]ModuleBlock, error) {
	data, err := os.ReadFile(filePath) //nolint:gosec // G304: path is provided by caller
	if err != nil {
		return nil, fmt.Errorf("failed to read file %s: %w", filePath, err)
	}

	parser := hclparse.NewParser()
	file, diags := parser.ParseHCL(data, filePath)
	if diags.HasErrors() {
		return nil, fmt.Errorf("failed to parse HCL file %s: %w", filePath, diags)
	}

	content, _, diags := file.Body.PartialContent(&hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: hclBlockTypeModule, LabelNames: []string{"name"}},
		},
	})
	if diags.HasErrors() {
		return nil, fmt.Errorf("failed to extract content from %s: %w", filePath, diags)
	}

	var modules []ModuleBlock
	for _, block := range content.Blocks {
		if block.Type != "module" {
			continue
		}

		if len(block.Labels) == 0 {
			continue
		}

		moduleName := block.Labels[0]

		// Extract the source attribute
		attrs, diags := block.Body.JustAttributes()
		if diags.HasErrors() {
			// If we can't get attributes, skip this module
			continue
		}

		sourceAttr, exists := attrs["source"]
		if !exists {
			// Module without source attribute
			modules = append(modules, ModuleBlock{
				Name:          moduleName,
				Source:        "",
				MissingSource: true,
			})
			continue
		}

		// Try to evaluate the source as a string literal
		val, diags := sourceAttr.Expr.Value(nil)
		if diags.HasErrors() {
			// Source is not a simple literal (could be a variable reference)
			// Skip this module
			continue
		}

		if val.Type() != cty.String {
			// Source is not a string
			continue
		}

		source := val.AsString()

		// Extract version attribute if present
		var version string
		if versionAttr, exists := attrs["version"]; exists {
			versionVal, versionDiags := versionAttr.Expr.Value(nil)
			if !versionDiags.HasErrors() && versionVal.Type() == cty.String {
				version = versionVal.AsString()
			}
		}

		modules = append(modules, ModuleBlock{
			Name:    moduleName,
			Source:  source,
			Version: version,
		})
	}

	return modules, nil
}
