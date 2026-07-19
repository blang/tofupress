# Strip levels: `full` / `optimistic` / `aggressive`, with the anchor rule

Status: proposed

## Context

tofupress's former stripping model ("strip modes": `none` / `module-dir` / `config-only`) was a **keep-set** filter that silently dropped files the resolver had no way to discover. The default `module-dir` trimmed the entry module's own non-`.tf` children unless a `file()`-family function with a *literal* argument pointed at them. This broke real, common configurations:

```hcl
# mods/moda/main.tf — the entry module
resource "null_resource" "go_lambda_build" {
  provisioner "local-exec" {
    command = "cd ${path.module}/src/hello-world && go build -o bootstrap main.go"
  }
}
module "hello-world-lambda" {
  source     = "terraform-aws-modules/lambda/aws"
  source_path = "${path.module}/src/hello-world"   # plain attribute — NOT a file() call
}
```

Under the old default, `moda/src/hello-world/main.go` was **silently stripped** because no `module` block referenced `src/`, and `source_path` is a plain attribute (only the `file()`/`fileset()`/`filemd5()`/`filebase64()`/`templatefile()` family was detected). The bundle looked fine; `tofu apply` failed at the `go build`.

A second defect compounded the first: the "dynamic `file()` fallback" (when a downloaded package contained a `file()` with a non-literal arg) set `IncludeAll` on the **broadest ancestor package root** rather than the module's own package — so a dynamic `filebase64sha256(...)` inside a vendored module accidentally kept the *user's* entire repo tree, masking the silent-loss bug by over-including.

## Decision

Replace the three legacy strip modes with three **strip levels** — `full`, `optimistic` (default), `aggressive` — and add an **anchor rule**. The rule is **uniform across the subject and downloaded packages** (no local-vs-remote distinction in the rule itself); only the *anchor set* differs by reachability.

### The anchor rule

A directory containing at least one `.tf` or `.tofu` file is an **anchor**. Anchors within the **press subject** are **preserved at their staged path** (never relocated or dropped) in every strip level except `aggressive`, because consumers may address them via `//` (e.g. `artifact.zip//examples/big-example`). An anchor's `.tf`/`.tofu` config files are kept; its **non-`.tf` content is not kept wholesale** — it follows the same keep-set as everything else (static `file()`/`fileset()` matches with literal args, and the whole owning package when the module is risk-flagged). The anchor rule is about **location invariance and config preservation**, not byte-for-byte wholesale retention of the directory's every file.

Directories inside the **vendor directory** are never anchors — consumers cannot `//`-address into `_vendor/*`, so vendored packages are governed by the risk-detector keep-set, not the anchor rule.

### Strip level ladder (monotonic: each level keeps ⊆ the one above)

```
full        : everything verbatim (tofu-literal: matches what `tofu init` keeps on disk)
optimistic  : anchor dirs preserved at path (subject only) + .tf/.tofu config kept
              + static file()/filebase64()/filemd5()/fileset()/templatefile() matches (literal arg)
              + whole owning package verbatim for any module with a risk signal
              ; trims unreferenced non-.tf files (in anchor dirs and elsewhere) without a
                static match or risk signal; trims non-anchor dirs and vendor extras without a risk signal
aggressive  : .tf/.tofu files only; trim everything else (warns on detected filesystem reads)
```

### Risk signals (the optimistic keep-set's escalation triggers)

A module triggers a risk signal when its config contains **either**:

1. A `file()`/`filebase64()`/`filemd5()`/`fileset()`/`templatefile()` call with a **non-literal argument** (e.g. `file("../shared/${var.name}.txt")`). Already detected today (`handlingDynamicFallback`).
2. **Any** string value of shape `${path.module}/...` or `${path.root}/...` appearing **anywhere** in the module's config — attribute values, `provisioner` `interpreter`/`command`/`working_dir`, `local-exec` commands, custom provider attributes, anywhere. This is the signal that catches `source_path = "${path.module}/src/hello-world"`, `interpreter = [..., "${path.module}/package.py", ...]`, and any future provider attribute that takes a module-relative path.

A module with a risk signal has its **whole owning package kept verbatim** (not just the referenced subtree), because the referenced file (e.g. `package.py`) may itself read arbitrary siblings at runtime — narrowing is unsafe.

### Owning package (reachability-based, not authorship-based)

- For a module **under the press subject**, the owning package is that module's directory subtree.
- For a module **under the vendor directory**, the owning package is the whole downloaded package `<vendorDir>/<packageID>`.

The fallback is scoped to the **module's own owning package** (the **narrowest** enclosing package root), fixing the former bug where a risk signal in a vendored module over-included the user's repo.

## Examples (pinning the contract, no ambiguity)

### Ex 1 — `tofupress module`, lambda `src/` survives under optimistic (default)

Subject `repo//modules/moda`, `moda/main.tf` as in the Context example. Archive root pivots to `moda`.

```
archive.zip
/main.tf                          # moda's config; source_path = "${path.module}/src/hello-world" → RISK SIGNAL
/src/hello-world/main.go          # KEPT — moda flagged risky → whole owning package (moda tree) verbatim
/_vendor/terraform-aws-modules/terraform-aws-lambda/main.tf   # remote entry .tf (config, kept)
# _vendor/.../examples/         TRIMMED — vendor, no anchor, no risk signal in lambda's code reaching examples
```

### Ex 2 — `tofupress tree`, addressable example dir stays put

Subject `repo` (no `//`). `repo/examples/big-example/main.tf` exists but no module references it.

```
archive.zip
/modules/moda/main.tf              # anchor dir (has .tf): dir preserved at path; .tf config kept
/modules/moda/README.md            # non-.tf in anchor dir → KEPT only if moda risk-flagged, else TRIMMED
/examples/big-example/main.tf     # anchor dir (has .tf within subject) → dir + .tf kept at /examples/big-example/ ← //examples/big-example works
/examples/big-example/lambda.go    # KEPT if big-example flagged risky; else TRIMMED
/docs/guide.md                     # TRIMMED — non-anchor dir (no .tf)
/_vendor/foo/...                   # vendor: keep-set + risk-detector, no anchor rule
```

### Ex 3 — null_resource with `package.py` (the case that forces whole-package escalation)

```hcl
resource "null_resource" "archive" {
  provisioner "local-exec" {
    interpreter = [local.python, "${path.module}/package.py", "build"]   # RISK SIGNAL
    command     = data.external.archive_prepare[0].result.build_plan_filename
  }
}
```

`${path.module}/package.py` is a risk signal → the module's whole owning package is kept verbatim. `package.py` survives, and so does anything else in the module dir that `package.py` may read (since `package.py`'s reads are not statically analyzable).

### Ex 4 — `aggressive` trims even risk-signaled content (with warning)

Same `moda` as Ex 1, `--strip=aggressive`:

```
archive.zip
/main.tf                           # .tf kept
# /src/hello-world/main.go         TRIMMED — aggressive keeps .tf only
# tofupress warns: "filesystem reads were detected; aggressive stripping may omit runtime files"
```

User explicitly chose the breakage risk for minimal size.

## Decision on the dropped middle level

A strip level named `module` was considered and **rejected**. It would have kept `.tf`-dirs verbatim + static `file()` matches but **not** applied the `${path.module}/...` risk-detector — i.e. its only distinctive behavior would be "trim content reachable solely via a `path.module` ref." That is precisely the unsafe behavior that breaks the lambda and the null_resource examples: there is no user who wants "lean but might silently break `local-exec`." When a module has no `path.module` refs at all, `optimistic` already degenerates to that level's behavior. The name also collided with the `tofupress module` command. Dropped.

## Consequences

- **Default behavior change** (alpha-permitted): bundles are larger by default than under the old `module-dir` default, because anchor dirs and risk-flagged owning packages are kept verbatim. Lean users pass `--strip=aggressive`; tofu-literal users pass `--strip=full`.
- **No silent data loss** for `path.module`-reachable content under the default. The lambda and null_resource classes are fixed.
- **The `PackageForModuleKey` scope bug must be fixed in the same change**: dynamic fallback must resolve to the module's **own** (narrowest) owning package, not an ancestor. Fixing the strip default without this would regress Ex 1 (the over-inclusion accident currently masks the silent loss).
- **Staging is already correct**: `stageBundle` copies whole subtrees per bucket (subject, local modules, vendor packages); only the strip *filter*'s scope and default were wrong. No resolver or bundler changes required for this ADR.
- Legacy strip-mode names (`none`, `module-dir`, `config-only`, `tf-only`) map: `none`→`full`, `module-dir`→`optimistic`, `config-only`/`tf-only`→`aggressive`. Migration via aliases is acceptable during alpha.

## Considered options

- **Keep the local-vs-remote distinction** (trim remote packages harder than user modules): rejected. `tofu init` keeps downloaded packages verbatim on disk (verified: 2.7MB for `terraform-aws-modules/lambda/aws`, including `examples/`, `tests/`, `.git/`); there is no asymmetry to mirror. The strip rule is uniform; only the anchor *set* differs by reachability (vendor is not `//`-addressable).
- **Attribute-name allowlist** for path-typed attrs (`source_path`, `filename`, …): rejected as brittle — misses custom provider attributes (`build_dir = "${path.module}/build"`). The value-shape heuristic (any `${path.module}/...` string) is provider-agnostic and future-proof; its cost is occasional over-include, which is the safe failure direction.
- **`module` strip level** (kept `.tf`-dirs but trimmed `path.module`-reachable non-`.tf` content): rejected as the unsafe middle (see above).
