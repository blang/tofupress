# tofupress

TofuPress recursively resolves OpenTofu/Terraform modules and presses them into a self-contained archive with no remote module sources left to fetch at runtime.

## Why

Terraform modules can form dependency trees of arbitrary depth. Consumers normally resolve that tree during `init`, while manually vendoring it clutters source repositories and is difficult to maintain. TofuPress resolves, rewrites, and deduplicates the tree once at release time so the resulting artifact is hermetic.

## Installation

```bash
go install github.com/blang/tofupress/cmd/tofupress@latest
```

## Usage

### Inspect a module tree

```bash
tofupress resolve ./my-infra
tofupress resolve ./my-infra --json
```

### Press one module

`module` selects one entry module and pivots it to the archive root:

```bash
tofupress module ./my-infra module.zip
```

The subject must contain a top-level `.tf` or `.tofu` file.

For a module inside a larger package, use `//` to select the entry explicitly:

```bash
# Package = ./my-monorepo; entry = infra/environments/prod
tofupress module ./my-monorepo//infra/environments/prod prod.zip
```

TofuPress does not infer a package by searching for a surrounding Git repository. Without `//`, the specified directory is the package boundary. This keeps parent/sibling inclusion explicit and prevents unrelated repository content from being copied. Internal symlinks are dereferenced for portability; dangling/cyclic links or targets outside that explicit boundary are refused.

Local module references to siblings can be relocated during the pivot. An entry module filesystem read that escapes through `../`, or a local module source that resolves to the package root itself, cannot preserve its meaning after the entry moves to archive root; TofuPress refuses those cases and directs you to `tree`.

### Press a tree of modules

`tree` preserves the subject layout without pivoting it. Every directory containing a `.tf` or `.tofu` file is an entry anchor:

```bash
tofupress tree ./my-monorepo monorepo.zip
```

Consumers select an anchor with Terraform's package-subdirectory syntax:

```hcl
module "prod" {
  source = "https://modules.example.com/monorepo.zip//infra/environments/prod"
}
```

Use `tree` for module repositories, multiple entry modules, and package-relative filesystem reads that depend on the original directory geometry.

### Artifact formats

| Format | Typical distribution |
|---|---|
| **zip** | OCI registry or HTTP |
| **tar.gz** | HTTP server |
| **tar.xz** | Object storage |

The output extension is used when `--format=auto` (the default), or the format can be explicit:

```bash
tofupress module ./my-infra module.tar.gz --format=tar.gz
tofupress tree ./my-monorepo modules.tar.xz --format=tar.xz
```

### Strip levels

The default `optimistic` level keeps configuration, statically detected filesystem inputs, and risk-signaled owning packages. Other levels are:

- `full`: retain all relevant package content.
- `aggressive`: retain configuration and statically proven inputs, even when runtime reads may be omitted.

```bash
tofupress module ./my-infra module.zip --strip=full
```

Legacy alpha aliases (`none`, `module-dir`, `config-only`, and `tf-only`) remain accepted and are recorded in metadata.

## Distribution

### OCI registry

Create an OCI-compliant ZIP and push it with [ORAS](https://oras.land/):

```bash
tofupress module ./my-infra module.zip --oci-compliant

oras push \
  --artifact-type=application/vnd.opentofu.modulepkg \
  registry.example.com/my-module:v1.0.0 \
  module.zip:archive/zip
```

```hcl
module "my_infra" {
  source = "oci://registry.example.com/my-module?tag=v1.0.0"
}
```

The artifact type must be `application/vnd.opentofu.modulepkg`. OCI reads are strict by default; `--strict-oci=false` is an explicit compatibility escape hatch.

### HTTP

```bash
tofupress module ./my-infra module.tar.gz
scp module.tar.gz webserver:/var/www/modules/
```

```hcl
module "my_infra" {
  source = "https://modules.example.com/module.tar.gz"
}
```

### S3 or other object storage

```bash
tofupress module ./my-infra module.tar.xz
aws s3 cp module.tar.xz s3://my-modules/module.tar.xz
```

```hcl
module "my_infra" {
  source = "s3::https://my-modules.s3.amazonaws.com/module.tar.xz"
}
```

## How it works

1. Finds `.tf` and `.tofu` configuration at the selected entry or entries.
2. Parses and classifies local, Git, registry, HTTP, S3, GCS, and OCI sources.
3. Resolves the graph breadth-first within explicit package boundaries.
4. Downloads remote packages under a private `_vendor` directory.
5. Rewrites remote references and relocates local module references when a module is pivoted.
6. Applies the selected strip policy and content-based deduplication.
7. Stages one canonical layout, embeds `.tofupress/meta.json`, and atomically commits the requested archive format.

The resulting artifact is self-contained: `tofu init` or `terraform init` does not need the original external module sources.
