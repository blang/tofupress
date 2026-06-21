package tofupress

import (
	"fmt"
	"os"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// RewriteModuleSource rewrites the source attribute of a specific module block
// in a .tf file to point to a new local path. It preserves formatting and comments.
func RewriteModuleSource(filePath, moduleName, newSource string) error {
	data, err := os.ReadFile(filePath) //nolint:gosec // G304: path is provided by caller
	if err != nil {
		return fmt.Errorf("failed to read file %s: %w", filePath, err)
	}

	file, diags := hclwrite.ParseConfig(data, filePath, hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return fmt.Errorf("failed to parse HCL file %s: %w", filePath, diags)
	}

	var targetBlock *hclwrite.Block
	for _, block := range file.Body().Blocks() {
		if block.Type() == hclBlockTypeModule {
			labels := block.Labels()
			if len(labels) > 0 && labels[0] == moduleName {
				targetBlock = block
				break
			}
		}
	}

	if targetBlock == nil {
		return fmt.Errorf("module %q not found in %s", moduleName, filePath)
	}

	targetBlock.Body().SetAttributeValue("source", cty.StringVal(newSource))

	newData := file.Bytes()
	if err := os.WriteFile(filePath, newData, 0o644); err != nil { //nolint:gosec // G306: 0644 is standard for source files
		return fmt.Errorf("failed to write file %s: %w", filePath, err)
	}

	return nil
}

// RewriteModuleSourceByOldSource rewrites module sources in a Terraform file by matching the old source value.
// It finds all module blocks with the specified old source and rewrites them to the new source.
func RewriteModuleSourceByOldSource(filePath, oldSource, newSource string) error {
	data, err := os.ReadFile(filePath) //nolint:gosec // G304: path is provided by caller
	if err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}

	file, diags := hclwrite.ParseConfig(data, filePath, hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return fmt.Errorf("failed to parse HCL: %w", diags)
	}

	modified := false
	for _, block := range file.Body().Blocks() {
		if block.Type() != hclBlockTypeModule {
			continue
		}

		sourceAttr := block.Body().GetAttribute("source")
		if sourceAttr == nil {
			continue
		}

		// Get the current source value
		tokens := sourceAttr.Expr().BuildTokens(nil)
		var currentSource string
		for _, token := range tokens {
			currentSource += string(token.Bytes)
		}
		// Remove quotes
		currentSource = strings.Trim(strings.TrimSpace(currentSource), "\"")

		// Check if it matches the old source
		if currentSource == oldSource {
			block.Body().SetAttributeValue("source", cty.StringVal(newSource))
			modified = true
		}
	}

	if modified {
		if err := os.WriteFile(filePath, file.Bytes(), 0o644); err != nil { //nolint:gosec // G306: standard permissions
			return fmt.Errorf("failed to write file: %w", err)
		}
	}

	return nil
}
