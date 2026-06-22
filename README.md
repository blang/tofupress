# tofupress

A CLI that takes an OpenTofu/Terraform root module, recursively resolves all referenced modules, and bundles everything into a single self-contained artifact — no external module sources left to resolve at runtime.

## Why

Terraform's module system is great for sharing code, but it creates a dependency tree of arbitrary depth that consumers must resolve at init time. The usual alternative — vendoring all modules into the repository — is tedious to manage and clutters git history, especially when those modules recursively pull in submodules of their own. **tofupress** lets you keep using modules the way Terraform intended while producing a single, hermetic artifact at release time. As a bonus, the recursive condensing step can deduplicate modules that appear multiple times in the tree.

## Installation

```bash
go install github.com/blang/tofupress/cmd/tofupress@latest
```

## Usage

### Resolve modules

Scan and resolve all module dependencies, displaying the resolved tree:

```bash
tofupress resolve ./my-infra
```

Output as JSON for programmatic use:

```bash
tofupress resolve ./my-infra --json
```

### Bundle modules

Resolve and create a self-contained archive:

```bash
tofupress bundle ./my-infra bundle.zip
```

#### Monorepo support

For monorepo layouts where Terraform root modules live deep inside a repository, use the `//` syntax to separate the package root from the entry subdirectory:

```bash
# The monorepo root is the package; infra/environments/prod is the entry point
# All ../ references resolve relative to the monorepo root
tofupress bundle ./my-monorepo//infra/environments/prod bundle.zip

# Same syntax works with resolve
tofupress resolve ./my-monorepo//infra/environments/prod --json
```

Without `//`, TofuPress only copies the specified directory. With `//`, the entire repository is the package root, so sibling and parent directory references (e.g., `../../modules/network`) resolve correctly.

#### Bundle formats

TofuPress supports multiple archive formats for different distribution channels:

| Format | Use Case | Distribution |
|--------|----------|--------------|
| **zip** (default) | OCI registries | `oras push` + `source = "oci://..."` |
| **tar.gz** | HTTP servers | `source = "https://..."` |
| **tar.xz** | S3/object storage | `source = "s3::https://..."` |

Specify the format with `--format`:

```bash
tofupress bundle ./my-infra bundle.tar.gz --format tar.gz
tofupress bundle ./my-infra bundle.tar.xz --format tar.xz
```

## Distributing bundles

### OCI registry distribution (recommended)

OpenTofu supports installing modules directly from OCI-compliant registries. TofuPress creates OCI-compatible ZIP bundles that you can push with [oras](https://oras.land/):

```bash
# 1. Create ZIP bundle
tofupress bundle ./my-infra bundle.zip

# 2. Push to OCI registry
oras push \
  --artifact-type=application/vnd.opentofu.modulepkg \
  registry.example.com/my-module:v1.0.0 \
  bundle.zip:archive/zip
```

Consumers can then use the module in their Terraform/OpenTofu configurations:

```hcl
module "my-infra" {
  source = "oci://registry.example.com/my-module?tag=v1.0.0"
}
```

**Note:** The `--artifact-type` flag must be set to exactly `application/vnd.opentofu.modulepkg` for OpenTofu to recognize the artifact as a module package.

### HTTP server distribution

For HTTP distribution, create a tar.gz bundle and host it on any web server:

```bash
# 1. Create tar.gz bundle
tofupress bundle ./my-infra bundle.tar.gz --format tar.gz

# 2. Upload to your web server
scp bundle.tar.gz webserver:/var/www/modules/
```

Consumers reference it via HTTPS:

```hcl
module "my-infra" {
  source = "https://modules.example.com/bundle.tar.gz"
}
```

### S3/object storage distribution

For S3 distribution, create a tar.xz bundle (smaller size) and upload to your bucket:

```bash
# 1. Create tar.xz bundle
tofupress bundle ./my-infra bundle.tar.xz --format tar.xz

# 2. Upload to S3
aws s3 cp bundle.tar.xz s3://my-modules/bundle.tar.xz
```

Consumers reference it via S3:

```hcl
module "my-infra" {
  source = "s3::https://my-modules.s3.amazonaws.com/bundle.tar.xz"
}
```

## How it works

1. **Discovery**: Scans all `.tf` files in the target directory
2. **Parsing**: Extracts `module` blocks and their `source` attributes
3. **Classification**: Determines the source type (local, git, registry, HTTP, S3, GCS, OCI)
4. **Resolution**: Recursively downloads remote modules using breadth-first search
5. **Deduplication**: Modules referenced multiple times are downloaded once
6. **Rewriting**: Updates `source` attributes to point to local `./sourcetree/` paths
7. **Bundling**: Creates an archive with the root module and all dependencies

The resulting bundle is completely self-contained — running `terraform init` or `tofu init` on it requires no network access to external module sources.
