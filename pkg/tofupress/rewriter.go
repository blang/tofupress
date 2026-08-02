package tofupress

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// ErrModuleBlockNotFound is returned by RewriteModuleSource when the named module block
// is not present in the target file. Callers use it to distinguish "block lives in a
// different .tf file" from genuine read/parse/write failures (review finding F8).
var ErrModuleBlockNotFound = errors.New("module block not found in file")

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

	targetBlock, err := findModuleBlock(file, moduleName)
	if err != nil {
		return err
	}

	targetBlock.Body().SetAttributeValue("source", cty.StringVal(newSource))

	newData := file.Bytes()
	if err := os.WriteFile(filePath, newData, 0o644); err != nil { //nolint:gosec // G306: 0644 is standard for source files
		return fmt.Errorf("failed to write file %s: %w", filePath, err)
	}

	return nil
}

// RewriteModuleSourceAndDropVersion rewrites the source attribute of a specific module
// block to a new local path AND removes its `version` attribute. It is used when
// rewriting a registry module's source (which carries `version = "x.y.z"`) to a local
// vendored path: OpenTofu/Terraform reject a `version` attribute on a non-registry
// source with `Invalid registry module source address` (review item 1).
//
// Provenance is preserved by leaving a `# version = "x.y.z" (pinned by tofupress)`
// trailing comment on the block when the original `version` value can be read.
func RewriteModuleSourceAndDropVersion(filePath, moduleName, newSource string) error {
	data, err := os.ReadFile(filePath) //nolint:gosec // G304: path is provided by caller
	if err != nil {
		return fmt.Errorf("failed to read file %s: %w", filePath, err)
	}

	file, diags := hclwrite.ParseConfig(data, filePath, hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return fmt.Errorf("failed to parse HCL file %s: %w", filePath, diags)
	}

	targetBlock, err := findModuleBlock(file, moduleName)
	if err != nil {
		return err
	}

	body := targetBlock.Body()

	// Capture the original version value for provenance BEFORE removing the attribute.
	commentTokens := versionProvenanceComment(body)

	body.SetAttributeValue("source", cty.StringVal(newSource))
	body.RemoveAttribute("version")

	if commentTokens != nil {
		body.AppendUnstructuredTokens(commentTokens)
	}

	newData := file.Bytes()
	if err := os.WriteFile(filePath, newData, 0o644); err != nil { //nolint:gosec // G306: 0644 is standard for source files
		return fmt.Errorf("failed to write file %s: %w", filePath, err)
	}

	return nil
}

// findModuleBlock locates the first `module "<moduleName>" { ... }` block in file.
func findModuleBlock(file *hclwrite.File, moduleName string) (*hclwrite.Block, error) {
	for _, block := range file.Body().Blocks() {
		if block.Type() != hclBlockTypeModule {
			continue
		}
		labels := block.Labels()
		if len(labels) > 0 && labels[0] == moduleName {
			return block, nil
		}
	}
	return nil, fmt.Errorf("module %q not found: %w", moduleName, ErrModuleBlockNotFound)
}

// versionProvenanceComment reads the `version` attribute of a block and, when it is a
// string literal, returns unstructured tokens for a trailing provenance comment of the
// form ` # version = "x.y.z" (pinned by tofupress)`. Returns nil when the attribute is
// absent or its value cannot be read as a string literal.
func versionProvenanceComment(body *hclwrite.Body) hclwrite.Tokens {
	attr := body.GetAttribute("version")
	if attr == nil {
		return nil
	}
	tokens := attr.Expr().BuildTokens(nil)
	var raw strings.Builder
	for _, tok := range tokens {
		raw.Write(tok.Bytes)
	}
	value := strings.TrimSpace(raw.String())
	value = strings.Trim(value, `"`)
	if value == "" {
		return nil
	}
	// hclsyntax.TokenComment marks a comment token (`# ...` / `// ...` / `/* ... */`).
	// SpacesBefore is preserved as 2 so the comment stays indented inside the block body.
	return hclwrite.Tokens{
		{
			Type:         hclsyntax.TokenComment,
			Bytes:        fmt.Appendf(nil, "# version = %q (pinned by tofupress)", value),
			SpacesBefore: 2,
		},
		{
			Type:  hclsyntax.TokenNewline,
			Bytes: []byte{'\n'},
		},
	}
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
