# `tofupress module` vs `tofupress tree` — two commands, not a flag

Status: implemented

Implemented on `feature/dev` (commits `6d033a0` phase 1, `3a09e0d` phase 2).
`bundle` is dropped (alpha clean cut); the replacement for the former
`bundle <dir> <out>` invocation is `tofupress module <dir> <out>` (there is no
runtime alias). ParseStripMode accepts the legacy aliases from
ADR-0001 unchanged. Two clarifications resolved during implementation are
normative for this ADR:

- **The shared pressing pipeline**: `module` and `tree` share one resolution
  core (`resolve(ctx, rootDir, multiEntry)`) and one pressing pipeline
  (`runPress`); the two commands are thin adapters differing only in the
  resolveFn (`Resolve` vs `ResolveTree`) and the package boundary. The ADR's
  "commands, not a flag" decision is honored at the CLI surface; internally
  the dimensions that differ (archive shape, entry validation, anchor set) are
  encoded as the seed strategy + boundary, not as post-resolution filters.
- **`tree` boundary = subject, not the `//`-expanded package root**.
  `resolveSource` returns the `//subdir`-expanded package root for `module`
  (so `../` from the entry resolves inside the package); `tree` overrides that
  with the subject itself, so a local ref escaping the subject is a boundary
  error. Pressing the layout as-is within the subject is the `tree` contract.
- **Local package boundaries are explicit**. An unqualified local source is
  copied as exactly that package; TofuPress does not sniff for and expand to a
  surrounding Git repository. Callers opt into parent/sibling package content
  with `package//entry`. In `module` mode, an entry filesystem read that escapes
  the selected entry cannot survive the archive-root pivot, so the press is
  refused with `tree` as the layout-preserving remedy. The same refusal applies
  when a local module source resolves to the package root: the selected entry and
  package-root module cannot both occupy archive root without one overwriting the
  other.
- **Per-module N-artifact output is deferred** as a future output flag on
  `tofupress tree` (pivot each discovered entry to its own archive root), as
  the ADR stated; this implementation presses one tree-shaped artifact per
  invocation.

## Context

tofupress today has one command, `bundle <dir> <output>`, that presses a single root module (pivot mode via `//subdir`). For a "modules-only" repository — a repo whose root has no `.tf` files but contains many modules under `modules/*` — there is no way to produce *one* hermetic artifact that preserves the repo's directory shape and lets consumers address any submodule via `//`. Pressing the repo root degenerates to an empty bundle (resolver descends nowhere). Pressing each `modules/*` separately yields N independent artifacts (workable but no cross-module dedup, and a CI loop). A `--all-submodules` flag was considered as the fix.

## Decision

Introduce two **commands** — `tofupress module` and `tofupress tree` — distinguished by the **subject's shape contract**, not by a flag on a single command. Drop the legacy `bundle` command entirely (alpha clean cut). The tool name *is* the verb (tofu-**press**), so the subcommands are the **objects** being pressed, reading as "tofupress (this) module" / "tofupress (this) tree" — mirroring `git clone`/`git pull`.

### `tofupress module <subject> [output]`

Presses **one module**. Pivots the entry module to the archive root. The subject must contain at least one `.tf`/`.tofu` file or the press is **refused** — a no-`.tf` subject is not a module; it belongs to `tofupress tree`.

Consumer usage: `source = "oci://registry/moda"` (no `//` — the entry is the archive root).

```
$ tofupress module repo//modules/moda moda.zip
archive.zip
/main.tf                  # moda pivoted to root
/modules/modb/...         # local dep, staged at package-rel path (moda referenced ../modb)
/_vendor/foo/...          # remote dep, vendored
```

### `tofupress tree <subject> [output]`

Presses the **subject as-is, unpivoted**. Every `.tf`/`.tofu`-bearing directory within the subject is an **anchor** (ADR-0001) kept at its staged path; consumers may address any of them via `//`. Local `../` cross-references between modules are preserved **verbatim** (the layout is unchanged, so `../modb` still resolves at the unpacked root — nothing to rewrite). Only remote/registry references are rewritten and vendored.

The resolver discovers **every `.tf`-dir under the subject as an entry** and resolves each, so that every entry's remote dependencies are discovered and vendored in a single pass (shared remote deps dedup by content hash). This replaces both the synthetic-root workaround and the per-module CI loop with one first-class, dedup-aware operation.

Consumer usage: `source = "oci://registry/repo//modules/moda"`, `source = "oci://registry/repo//examples/big-example"`.

```
$ tofupress tree myrepo repo.zip
archive.zip
/modules/moda/main.tf             # as-is (shape preserved)
/modules/modb/main.tf              # as-is; moda's source="../modb" still works (layout unchanged)
/examples/big-example/main.tf      # anchor under subject → kept at path → //examples/big-example works
/README.md                         # trimmed under optimistic (non-anchor file in subject)
/_vendor/foo/...                   # remote deps, vendored & rewritten
```

### Why commands (and these names), not a `--all-submodules` flag

The two commands differ in three independently-meaningful dimensions, all driven by the subject's shape contract:

1. **Archive shape**: `module` pivots (entry → `/`); `tree` is unpivoted (subject as-is). A flag overloads one archiver with two shapes.
2. **Entry validation**: `module` refuses a no-`.tf` subject; `tree` requires the subject to be a tree (may have `.tf`-dirs anywhere). Different preconditions, not a flag toggle.
3. **Anchor set**: `tree` anchors every `.tf`-dir under the subject; `module` anchors only the entry + resolver-descended deps (the entry is the sole user-facing addressable surface). This is a different *resolution* strategy (multi-entry discovery), not a post-resolution filter flag.

A flag conflates these; two commands make the subject contract explicit. The tool name *is* the verb (tofu-**press**), so the subcommands are the *objects* (`module`, `tree`) — not a redundant `press` verb prefix (`tofupress press module` reads as "tofu-press press module").

## The lambda anchor subtlety (why `tofupress tree` + optimistic is coherent)

Under `tofupress tree`, a stray `scripts/deploy.tf` five levels deep is also an anchor and is kept verbatim — that is the deal of pressing the tree ("repo as-is in shape"). If the user wanted only `modules/*`, they pass `repo//modules` as the subject (a `tofupress tree` of a `//subdir` is meaningful: subject = `modules/`, tree-shaped, unpivoted within that boundary). Optimistic's trimming is in **non-anchor, non-risk-signaled** content only — narrower in `tree` mode than in `module` mode by design, because the subject's contract preserves more structure. Users wanting lean per-module artifacts use `tofupress module`.

## Consequences

- The `bundle` command is **dropped** (alpha clean cut). The former `bundle <dir> <out>` spelling must be changed to `tofupress module <dir> <out>`; no alias is registered.
- `tofupress tree`'s multi-entry resolution is new resolver machinery (discover every `.tf`-dir under subject as an entry; merge trees; dedup local modules by InstallDir and remote packages by content hash). The archiver is largely reusable: `tree` = unpivoted staging of the subject as-is + vendor dir (no pivot, no `repivotMonorepoSources`). The per-module N-artifact output (pivot each discovered entry to its own archive root) is a **future output flag** on `tofupress tree`, not part of this ADR.
- `tofupress module`'s refusal of a no-`.tf` subject is a behavior change from `bundle` (which only warned). Justified: a no-`.tf` "module" is the modules-repo case, which `tofupress tree` is the correct tool for; warning-and-degenerating silently produced broken/empty bundles.
- Cross-module local refs (`moda` → `../modb`) in `tofupress tree` are **not rewritten** (layout preserved); in `tofupress module` they **are** repivoted (`../modb` → `./modules/modb`) because the entry is flattened to the archive root. This asymmetry is deliberate and follows from the pivot decision.

## Considered options

- **`--all-submodules` flag on a single press command**: rejected — overloads one archiver with two archive shapes, two validation rules, and two resolution strategies; the subject-kind contract is clearer as a command.
- **`press module`/`press tree` prefix verb**: rejected — the tool is *already* named tofupress; `tofupress press module` doubles the verb redundantly. Subcommands are the objects (`module`, `tree`).
- **Keep `bundle` as alias**: rejected — redundant with the tool-name-as-verb model and alpha clean cut.
- **`module-tree` over `tree`**: rejected — `tree` is shorter and self-explanatory given the sibling `module`; `resolve` already owns "show tree", so no collision.
- **Synthetic-root workaround** (generate a dummy `main.tf` referencing all `modules/*`, press): works today, is hermetic, but requires CI maintenance (regenerate when modules are added/removed), ships dead bytes (the dummy root) inside the artifact, and is not dedup-aware across independent resolutions. Replaced by `tofupress tree`'s single-pass multi-entry resolution.
- **Per-module CI loop** (`tofupress module repo//modules/<x>` for each `x`): works today, hermetic per artifact, but N independent resolutions (no cross-module dedup of shared remote deps) and registry sprawl (N artifacts). `tofupress tree` with the future per-module output flag collapses the loop into one dedup-aware invocation.
- **Default strip level divergence by command** (`module`→`optimistic`, `tree`→`full`): rejected. With the anchor rule from ADR-0001, `optimistic` is correct for both commands — `tree`'s broader anchor set makes it naturally preserve more structure. The strip level is orthogonal to the command; default is `optimistic` for both.
