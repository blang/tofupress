package tofupress

import (
	"fmt"
	"os"

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
