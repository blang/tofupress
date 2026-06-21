package tofupress

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

const handlingDynamicFallback = "dynamic-package-fallback"
const handlingStaticInclude = "static-include"

var terraformFilesystemFunctions = map[string]struct{}{
	"file":             {},
	"filebase64":       {},
	"fileexists":       {},
	"fileset":          {},
	"templatefile":     {},
	"filemd5":          {},
	"filesha1":         {},
	"filesha256":       {},
	"filesha512":       {},
	"filebase64sha256": {},
	"filebase64sha512": {},
}

// FilesystemFunctionRef records one Terraform/OpenTofu filesystem-function call.
type FilesystemFunctionRef struct {
	Function      string   `json:"function"`
	ModuleKey     string   `json:"module_key"`
	SourceFile    string   `json:"source_file"`
	SourceRange   string   `json:"source_range"`
	RawPath       string   `json:"raw_path,omitempty"`
	RawPattern    string   `json:"raw_pattern,omitempty"`
	ResolvedBase  string   `json:"resolved_base,omitempty"`
	Handling      string   `json:"handling"`
	IncludedPaths []string `json:"included_paths,omitempty"`
	Static        bool     `json:"static"`
}

// DetectTreeFilesystemFunctions detects filesystem reads across all modules in a resolved tree.
func DetectTreeFilesystemFunctions(tree *ResolvedTree) ([]FilesystemFunctionRef, error) {
	if tree == nil || tree.Root == nil {
		return nil, fmt.Errorf("cannot detect filesystem functions in empty tree")
	}
	rootDir := tree.Root.InstallDir
	refs := make([]FilesystemFunctionRef, 0)
	for _, module := range tree.AllModules {
		moduleRefs, err := DetectModuleFilesystemFunctions(rootDir, module)
		if err != nil {
			return nil, err
		}
		refs = append(refs, moduleRefs...)
	}
	return refs, nil
}

// DetectModuleFilesystemFunctions detects filesystem-function calls in one module's .tf/.tofu files.
func DetectModuleFilesystemFunctions(rootDir string, module *ModuleNode) ([]FilesystemFunctionRef, error) {
	if module == nil {
		return nil, fmt.Errorf("module is nil")
	}
	files, err := FindTerraformFiles(module.InstallDir)
	if err != nil {
		return nil, err
	}

	ctx := pathEvalContext{RootDir: rootDir, ModuleDir: module.InstallDir}
	refs := make([]FilesystemFunctionRef, 0)
	for _, filePath := range files {
		data, err := os.ReadFile(filePath) //nolint:gosec // path comes from resolved module tree
		if err != nil {
			return nil, fmt.Errorf("failed to read %s: %w", filePath, err)
		}
		file, diags := hclsyntax.ParseConfig(data, filePath, hcl.Pos{Line: 1, Column: 1})
		if diags.HasErrors() {
			return nil, fmt.Errorf("failed to parse HCL file %s: %w", filePath, diags)
		}
		syntaxBody, ok := file.Body.(*hclsyntax.Body)
		if !ok {
			return nil, fmt.Errorf("parsed HCL file %s did not produce a syntax body", filePath)
		}
		walkFilesystemNodes(syntaxBody, module, filePath, ctx, &refs)
	}

	sort.Slice(refs, func(i, j int) bool {
		if refs[i].SourceFile == refs[j].SourceFile {
			return refs[i].SourceRange < refs[j].SourceRange
		}
		return refs[i].SourceFile < refs[j].SourceFile
	})
	return refs, nil
}

// pathEvalContext holds the root and module directories for path evaluation.
type pathEvalContext struct {
	RootDir   string
	ModuleDir string
}

// walkFilesystemNodes traverses an HCL syntax node tree and records filesystem-function calls.
func walkFilesystemNodes(node hclsyntax.Node, module *ModuleNode, filePath string, ctx pathEvalContext, refs *[]FilesystemFunctionRef) {
	_ = hclsyntax.VisitAll(node, func(visited hclsyntax.Node) hcl.Diagnostics {
		call, ok := visited.(*hclsyntax.FunctionCallExpr)
		if !ok {
			return nil
		}
		if _, isFilesystem := terraformFilesystemFunctions[call.Name]; !isFilesystem {
			return nil
		}
		*refs = append(*refs, buildFilesystemRef(call, module, filePath, ctx))
		return nil
	})
}

// buildFilesystemRef constructs a FilesystemFunctionRef from a function call expression.
func buildFilesystemRef(call *hclsyntax.FunctionCallExpr, module *ModuleNode, filePath string, ctx pathEvalContext) FilesystemFunctionRef {
	ref := FilesystemFunctionRef{
		Function:    call.Name,
		ModuleKey:   module.Key,
		SourceFile:  filePath,
		SourceRange: call.Range().String(),
		Handling:    handlingDynamicFallback,
	}
	if len(call.Args) == 0 {
		return ref
	}

	rawSource := expressionSourceText(call.Args[0])
	evaluated, ok := staticString(call.Args[0], ctx)
	if !ok {
		return ref
	}
	ref.Static = true
	ref.RawPath = rawSource
	ref.ResolvedBase = resolveTerraformPath(evaluated, ctx.ModuleDir)
	ref.Handling = handlingStaticInclude

	if call.Name == "fileset" {
		if len(call.Args) < 2 {
			ref.Static = false
			ref.Handling = handlingDynamicFallback
			return ref
		}
		rawPattern, patternOK := staticString(call.Args[1], ctx)
		if !patternOK {
			ref.Static = false
			ref.RawPattern = ""
			ref.Handling = handlingDynamicFallback
			return ref
		}
		ref.RawPattern = rawPattern
		matches, err := matchTerraformFileset(ref.ResolvedBase, rawPattern)
		if err != nil {
			ref.Static = false
			ref.Handling = handlingDynamicFallback
			return ref
		}
		ref.IncludedPaths = matches
		return ref
	}

	ref.IncludedPaths = []string{ref.ResolvedBase}
	return ref
}

// expressionSourceText returns the raw source text of an HCL expression from its range,
// with surrounding HCL string quotes stripped.
func expressionSourceText(expr hclsyntax.Expression) string {
	rng := expr.Range()
	if rng.Filename == "" {
		return ""
	}
	data, err := os.ReadFile(rng.Filename) //nolint:gosec // filename comes from parsed HCL file
	if err != nil {
		return ""
	}
	if rng.End.Byte > len(data) || rng.Start.Byte > len(data) {
		return ""
	}
	raw := string(data[rng.Start.Byte:rng.End.Byte])
	// Strip surrounding HCL string quotes (both "double" and heredoc)
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		raw = raw[1 : len(raw)-1]
	}
	return raw
}

// staticString evaluates an HCL expression with a known path context.
// Returns the string value and true if the expression is a static string literal.
func staticString(expr hclsyntax.Expression, ctx pathEvalContext) (string, bool) {
	evalCtx := &hcl.EvalContext{Variables: map[string]cty.Value{
		"path": cty.ObjectVal(map[string]cty.Value{
			"module": cty.StringVal(filepath.ToSlash(ctx.ModuleDir)),
			"root":   cty.StringVal(filepath.ToSlash(ctx.RootDir)),
			"cwd":    cty.StringVal(filepath.ToSlash(ctx.RootDir)),
		}),
	}}
	value, diags := expr.Value(evalCtx)
	if diags.HasErrors() || value.IsNull() || !value.IsWhollyKnown() || value.Type() != cty.String {
		return "", false
	}
	return value.AsString(), true
}

// resolveTerraformPath resolves a Terraform-style path to an absolute filesystem path.
func resolveTerraformPath(raw, moduleDir string) string {
	if filepath.IsAbs(raw) {
		return filepath.Clean(filepath.FromSlash(raw))
	}
	return filepath.Clean(filepath.Join(moduleDir, filepath.FromSlash(raw)))
}

// matchTerraformFileset walks a directory tree and matches files against a Terraform fileset pattern.
func matchTerraformFileset(baseDir, pattern string) ([]string, error) {
	pattern = path.Clean(filepath.ToSlash(pattern))
	matches := make([]string, 0)
	walkErr := filepath.WalkDir(baseDir, func(filePath string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case dirNameTerraform, dirNameGit:
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(baseDir, filePath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		ok, err := matchFilesetPattern(pattern, rel)
		if err != nil {
			return err
		}
		if ok {
			matches = append(matches, filePath)
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	sort.Strings(matches)
	return matches, nil
}

// matchFilesetPattern matches a relative path against a Terraform fileset glob pattern.
func matchFilesetPattern(pattern, rel string) (bool, error) {
	if ok, err := path.Match(pattern, rel); err == nil && ok {
		return true, nil
	}
	if suffix, found := strings.CutPrefix(pattern, "**/"); found {
		if ok, err := path.Match(suffix, path.Base(rel)); err != nil || ok {
			return ok, err
		}
	}
	if strings.Contains(pattern, "/**/") {
		parts := strings.SplitN(pattern, "/**/", 2)
		prefix, suffix := parts[0], parts[1]
		if remaining, found := strings.CutPrefix(rel, prefix+"/"); found {
			if ok, err := path.Match(suffix, path.Base(remaining)); err != nil || ok {
				return ok, err
			}
			return path.Match(suffix, remaining)
		}
		return false, nil
	}
	return path.Match(pattern, rel)
}
