# TofuPress

A CLI that takes an OpenTofu/Terraform root module, recursively resolves all referenced modules, and bundles everything into a single self-contained archive artifact. This glossary pins the vocabulary used across the codebase, ADRs, and CLI surface.

## Language

**Press**:
The act of resolving a subject's module dependencies and producing a self-contained archive. The tool name *is* the verb (tofu-**press**); commands are `tofupress module` and `tofupress tree`.
_Avoid_: bundle (legacy command name — dropped in alpha), package, export.

**Subject**:
The path the user nominated to press. Everything at or below this path is in-scope; everything above it is excluded. Bound by the `//` package-subdir separator (`repo//modules/moda` → subject is `modules/moda`, package is `repo`).
_Avoid_: root (overloaded — see Archive root, Entry module), input, source.

**Module**:
A single directory containing at least one `.tf` or `.tofu` configuration file. Terraform/OpenTofu's unit of configuration; non-recursive (a module is the `.tf` files in one directory, not its subdirectories).
_Avoid_: package (a package is a downloaded unit, see Downloaded package), submodule.

**Entry module**:
The module directory the user pointed the press at (the subject itself, when it contains `.tf`). For `tofupress module`, the entry must contain a `.tf` file or the press is refused.
_Avoid_: root module (overloaded), main module.

**Downloaded package**:
A remote module's full downloaded tree (from git, HTTP, registry, S3, OCI), staged under the vendor directory in the archive. Distinct from a Module: a package may contain many modules (entry dir + submodules + examples/tests).
_Avoid_: vendored module (a package may hold many modules), remote module.

**Vendor directory**:
The directory in the archive that holds downloaded packages, rooted at `<vendorDir>/<packageID>/...`. Default name `_vendor` (configurable via `--vendor-dir`). Not addressable by consumers via `//`.
_Avoid_: sourcetree (legacy name, pre-rename), modules dir, vendor.

**Package root**:
The filesystem boundary within which local `../` references may resolve without error. Set by the `//` separator: left of `//` is the package, right is the entry subdir. Enforced by the resolver (`ensureWithinPackage`).
_Avoid_: repo root (may differ from package root in `//subdir` mode).

**Archive root**:
The top-level directory of the produced archive; what a consumer sees when they unpack. For `tofupress module`, the archive root pivots to the entry module (`entry/main.tf` → `/main.tf`). For `tofupress tree`, the archive root is the subject as-is (no pivot).
_Avoid_: root (overloaded).

**Pivot**:
The flattening of the entry module to the archive root, so the entry's files land at `/` in the archive rather than at `<subject-relative path>/`. Happens in `tofupress module`; does not happen in `tofupress tree`.
_Avoid_: flatten, repivot (internal staging term).

**Anchor**:
A directory containing at least one `.tf`/`.tofu` file, within the press subject. Anchors are kept verbatim at their staged path in every strip level except `aggressive`, because consumers may address them via `//`. A directory inside the vendor directory is never an anchor (vendor is not `//`-addressable).
_Avoid_: module dir (an anchor is a `.tf`-bearing dir; a module is its `.tf` files), keep-dir.

**Strip level**:
The policy selecting which staged files survive into the archive. Three levels: `full`, `optimistic`, `aggressive`. Applied uniformly to the subject and to downloaded packages — no local-vs-remote distinction in the rule, only in which directories count as anchors (subject only, not vendor).
_Avoid_: strip mode (implementation term), filter.

**Risk signal**:
A static indicator that a module reads filesystem content tofupress cannot precisely resolve. Two kinds: (1) a `file()`/`filebase64()`/`filemd5()`/`fileset()`/`templatefile()` call with a non-literal (dynamic) argument; (2) any string value of shape `${path.module}/...` or `${path.root}/...` appearing anywhere in the module's config. A module with a risk signal has its whole owning package kept verbatim under `optimistic`.
_Avoid_: heuristic (only the second kind is heuristic; the first is precise), unsafe read.

**Owning package**:
The package whose tree is kept verbatim when one of its modules triggers a risk signal. For a module under the press subject, the owning package is that module's directory subtree. For a module under the vendor directory, the owning package is the whole downloaded package `<vendorDir>/<packageID>`. Determined by reachability, not by authorship.
_Avoid_: module tree, containing package.

## Commands (axis 1 — what to press)

**`tofupress module <subject> [output]`**:
Presses one module. Pivots the entry module to the archive root. The subject must contain a `.tf`/`.tofu` file or the press is refused (a no-`.tf` subject belongs to `tofupress tree`). The tool name *is* the verb (tofu-**press**); the subcommand is the object being pressed.
_Avoid_: press module, bundle (dropped — legacy command, alpha clean cut), press one.

**`tofupress tree <subject> [output]`**:
Presses the subject as-is, unpivoted. Every `.tf`/`.tofu`-bearing directory within the subject is an anchor kept at its staged path; consumers may address any of them via `//`. Local `../` cross-references between modules are preserved verbatim (layout unchanged). Only remote/registry references are rewritten and vendored.
_Avoid_: press tree, press repo, press all, --all-submodules (rejected — see ADR-0002), module-tree (rejected — `tree` is shorter).

## Strip levels (axis 2 — what survives)

**`full`**:
Keep everything verbatim — every file of the subject and every downloaded package wholesale. Tofu-literal: matches what `tofu init` keeps on disk.
_Avoid_: none (legacy name; reads as "disabled/unsafe" but `full` is the tofu-faithful baseline).

**`optimistic`** (default):
Keep every `.tf`/`.tofu`-bearing directory in the subject verbatim (anchor rule) + statically-resolved `file()`/`fileset()`/`filemd5()`/`filebase64()`/`templatefile()` matches with literal args + whole owning package verbatim for any module with a risk signal. Trim unreferenced non-`.tf` files and non-anchor directories. Applied uniformly to subject and vendor (vendor has no anchors; risk-detector keep-set applies there).
_Avoid_: module-dir (legacy name of the unsafe former default), smart, hybrid.

**`aggressive`**:
Keep `.tf`/`.tofu` files only; trim everything else, including risk-signaled content. Warns when filesystem reads are detected. Opt-in for users who accept the breakage risk for minimal size.
_Avoid_: config-only, tf-only (legacy names — undersell how destructive this is), insane.
