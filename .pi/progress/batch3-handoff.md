# Batch 3 Handoff — v1 release blockers

**Branch:** `feature/dev`
**Date:** 2026-07-06
**Base:** batch 2 handoff (`9610827 refactor(bundler): make aggregation non-destructive`)
**Master review:** `.pi/reviews/2026-07-05-v1-release-review.md`

## What this batch shipped (7 commits, in fix order)

```
0bb95ff feat(bundler): namespaced OCI vendor layout _vendor/<ns>/<name>/ (item 6)
0a6c01e feat(resolve): add --out plan file (item 8 minimal landing)
2c2cbe2 feat(bundler): relocate meta.json out of the package root to .tofupress/ (item 6)
1464d7c fix(oci-getter): strict artifactType + --strict-oci flag (item 10)
babac81 feat(cli): operability pass — bundle --json, version --json, SIGINT cleanup, dir-output hint (item 9)
2298ab7 fix(strip): warn loudly when module-dir excludes a remote package sibling (item 4)
507d613 refactor(api): options pattern + ctx threading + injectable fetcher (item 5 companion)
```

### Verification baseline this batch left green
- `just lint` → 0 issues.
- `go test ./...` → 2 pkgs ok (non-integration suite green; batch-2 baseline 378 PASS / 0 FAIL maintained).
- `go test -tags=integration -count=1 ./... -run 'TestAcceptanceConsumeMatrix|TestIntegration_CrossRepoMutualDiamond|TestAcceptance_Monorepo'` → 30 pass / 0 fail / 10 skip (skips = `--oci-compliant × {tar.gz,tar.xz}` cells the bundler rejects by design).
- Item 9 SIGINT manual repro: 0 orphaned `/tmp/tofupress-*` dirs after SIGINT mid-bundle.
- Item 6 e2e smoke: `meta.json` only at `./.tofupress/meta.json`; `tofu init` + `tofu validate` succeed at the unpacked root.

---

## Per-item status

### Item 5 part 2 — LANDED (full)
**Commit:** `507d613`

- Options pattern on `Resolver` and `Bundler`: `NewResolver(opts...)` / `NewBundler(format, opts...)` with functional options (`WithFetcher`, `WithHTTPClient`, `WithRoundTripper`, `WithConcurrency`, `WithMaxDepth/Modules`, `WithResolverVendorDir`, `WithPackageRoot`, `WithRootDir`, `WithProgress`, `WithOCICompliant`, `WithMetadata`, `WithStripPlan`, `WithVendorDir`). Exported fields remain settable for backwards compat.
- `context.Context` threaded through `Bundle`, `PlanStripping`, `BuildSourcetreeIdentityPlan`, `ApplySourcetreeIdentityPlan` and forwarded into the format writers (`bundleZipFromDir`/`bundleTarGzFromDir`/`bundleTarXzFromDir`) as a cancellation seam. `Bundle` defaults nil ctx to `Background`.
- Injectable `Fetcher` + `http.RoundTripper`: `NewFetcher(WithRoundTripper(...), WithGetters, WithDetectors)`; the OCI getter carries `roundTripper` and applies it in `authClient`. The registry client (`queryRegistryAPI`) now takes an `*http.Client` and the Resolver mirrors the fetcher's RoundTripper into it when no explicit client was set.
- One gotcha: `WithVendorDir` lives on the Bundler; the Resolver variant is `WithResolverVendorDir` (name collision avoidance).

### Item 4 — LANDED (warn-only decision)
**Commit:** `2298ab7`

**Decision (review offered mirror-none vs config-only vs warn-only): pick WARN-ONLY.**
`warnExcludedRemotePackageDirs` walks each downloaded remote package's top-level dirs and warns when a dir the plan would exclude (a sibling the resolver never descended) is found — names the package, the dir, and points at `--strip=none`. Generated/VCS dirs (.terraform, .git) are skipped.
mirror-none would have regressed the strip contract + the acceptance matrix; config-only is already exposed via `--strip=config-only`. The safe fallback `--strip=none` exists for users who hit the warning.

### Item 9 — LANDED (partial)
**Commit:** `babac81`

Landed:
- `bundle --json`: emits artifact metadata as indented JSON to stdout after success; human status prints suppressed on `--json`.
- `version --json`: emits `{"version","commit","build_time"}`.
- SIGINT/SIGTERM cleanup: `installSignalCleanup` in `pkg/cmd` wired into `runBundle`/`runResolve`. Once-guarded. Manual repro: 0 orphaned `/tmp/tofupress-*` dirs after SIGINT mid-bundle.
- Directory-shape output hint (QA-12): trailing-separator output gets "looks like a directory; tofupress writes an archive file" hint appended to the format-inference error.

**Deferred (item 9 remainder):**
- `--debug` real `slog.Debug` calls at key decision points in `pkg/` (strip decisions, dedup, symlink-skip, source classification). `--debug` currently sets `slog.LevelDebug` but there are zero `slog.Debug` calls in `pkg/`.
- Progress output during archive write (`bundleZipFromDir`/`bundleTarGzFromDir`/`bundleTarXzFromDir` at `bundler.go:~240,~340,~430`). Silent `filepath.Walk`; large bundles appear hung.

### Item 10 — LANDED (partial)
**Commit:** `1464d7c`

Landed:
- Strict `artifactType` (default): empty or non-matching `artifactType` rejected. Lenient mode (`--strict-oci=false`) accepts with a stderr warning. Extracted into a pure, testable `enforceArtifactType(strict, ref, artifactType)`.
- `--strict-oci` CLI flag (default true) on `bundle` and `resolve`, wired through `NewFetcher(WithStrictOCI(...))` → `NewResolver(WithFetcher(...))`.
- `NewFetcher` constructs `strictOCI: true` by default.

**Deferred (item 10 remainder):**
- Per-stage `context.WithTimeout` + retry with backoff around `repo.Fetch`/`repo.FetchReference` (mirror `queryRegistryAPI`'s retry in `resolver.go`).
- Persist `ManifestDigest`/`LayerDigest` into `PackageMetadata` (currently `verifyBlobDigest` checks the layer digest but discards it; metadata stores only `DownloadedHash`/`FinalHash`).
- Replace `RewriteModuleSourceByOldSource` raw-token matching (`rewriter.go:82-94`) with `hclwrite`/`hclsyntax` value comparison.
- Configurable max-blob-size (zip-bomb guard) on `io.Copy(io.MultiWriter(tmpZip, h), rc)` (`oci_getter.go:~182`) and the zip/tar extractors (`tempdir.go:~396-524`).

### Item 6 — LANDED (full, two commits)
**Commits:** `2c2cbe2` (meta.json relocation) + `0bb95ff` (namespaced layout)

**meta.json relocation (`2c2cbe2`):**
- New constants `MetadataDir` (`.tofupress`) and `MetadataRelPath`.
- `stageBundle` writes `meta.json` into `.tofupress/` inside the staging tree.
- `BuildArtifactMetadata` records `MetadataPath: MetadataRelPath`.
- Readers (zip, tar.gz, tar.xz, `ReadMetadataFromDir`) prefer `.tofupress/meta.json` and FALL BACK to the legacy root `meta.json` so pre-existing bundles remain readable. The tar reader is single-forward-pass: it buffers the legacy entry and prefers the relocated one at EOF.

**Namespaced vendor layout (`0bb95ff`):**
- **Decision (review offered namespaced + content-addressed; dedup by canonical address):** keep the dedup key content-addressed (`SourcetreeIDFromHash` keyed `ByFinalID`) so identical content always collapses to one canonical directory; derive the VISIBLE final ID from the package address:
  - `git::https://host/namespace/name.git` → `namespace/name`
  - registry `namespace/name/provider` → `namespace/name`
  - everything else (file://, oci://, s3://, gcs://, http archives, local paths) → `pkg-<sha>` fallback.
- On collision (same `<ns>/<name>`, different content) a short content-hash suffix disambiguates.
- `validateVendorDir` updated to recognize namespace top-level entries (`_vendor/example/` holding `_vendor/example/pkg-one/`) instead of falsely flagging the namespace dir as user-content conflict.
- Consumers can now reference sub-modules by name: `source = "./_vendor/terraform-aws-modules/vpc//modules/sub"`.

### Item 8 — LANDED (half)
**Commit:** `0a6c01e`

Landed:
- `resolve --out=plan.json` writes the resolution as JSON to a file (for CI inspection/archival). Reuses the existing `--json` output (`outputJSON`).

**Not landed — the content-addressed git cache + `bundle --from-resolution`:**
- `bundle --from-resolution=plan.json` is NOT supported. A separate CLI process cannot reconstruct the resolved tree from a plan file alone because the downloaded packages live in ephemeral temp dirs cleaned after `resolve` returns.
- The `--out` help text says this explicitly to avoid misleading users.

---

## What is next — follow-up queue for the next batch

In rough priority order. Each item below is genuinely a contained piece; none should regress the green matrix if landed carefully.

### 1. Item 8 — content-addressed git cache + `bundle --from-resolution` (HIGHEST PRIORITY)
The single remaining "new subsystem" piece. Requires:
- A content-addressed git cache at `~/.cache/tofupress/git/<digest>` so re-clones of the same ref are local (mirror the resolver's existing `git::` download path to copy from the cache first, falling back to a real clone on miss).
- `bundle --from-resolution=plan.json` that loads the plan, reconstructs the `ResolvedTree` from the plan + cached content, and skips `Resolve`.
- Need to decide whether the plan file stores enough to reconstruct the tree (currently it stores a cycle-safe JSON summary, not full `ModuleNode`/`DownloadedPackage` structs — a richer plan format or a separate tree-state file may be needed).
- The `resolve --out` help text already says `bundle --from-resolution` is not yet supported — update it when this lands.

### 2. Item 9 remainder — operability cluster
- `--debug` real `slog.Debug` calls at key decision points in `pkg/`:
  - strip decisions (`strip.go` `includeResolvedModuleDirs`, `warnExcludedRemotePackageDirs`)
  - dedup (`sourcetree_identity.go` `BuildSourcetreeIdentityPlan`)
  - symlink-skip, source classification (`scanner.go`, `source.go`)
- Progress output during archive write in the three `*FromDir` writers (`bundler.go:~240,~340,~430`). Print "Adding files…" / "Writing archive…" or a file count during the `filepath.Walk`.

### 3. Item 10 remainder — OCI getter strictness cluster
- Per-stage `context.WithTimeout` around `repo.Fetch`/`repo.FetchReference` in `oci_getter.go` Get/fetchAndDecompress. Add retry with backoff for transient transport errors (mirror `queryRegistryAPI`'s retry pattern in `resolver.go`).
- Persist `ManifestDigest`/`LayerDigest` into `PackageMetadata` (add fields to `PackageMetadata` in `metadata.go`; populate in `oci_getter.go` fetchAndDecompress after the digest is verified).
- Replace `RewriteModuleSourceByOldSource` raw-token matching (`rewriter.go:82-94`) with `hclwrite`/`hclsyntax` value comparison. The scanner-side heredoc path is already handled; this is the bundler-aggregation correctness edge only.
- Configurable max-blob-size (zip-bomb guard) on `io.Copy` in `fetchAndDecompress` (`oci_getter.go:~182`) and the zip/tar extractors (`tempdir.go:~396-524`). Add a `WithMaxBlobSize` FetcherOption + a CLI flag; reject on exceed.

---

## Conventions / guardrails (carry over)

- Use `mise` for ALL tools; `just` as the task runner; `prek` for git hooks.
- **prek amends files**: trailing-whitespace/EOF fixes mean the first commit may fail; re-`git add` and re-commit.
- golangci-lint v2, complexity thresholds gocyclo 15 / gocognit 20 — add `//nolint:gocognit,gocyclo` with a real reason if a function genuinely needs it.
- Run `just lint` + `just test` after each item, not just at the end. Run `go test -tags=integration -count=1 ./... -run 'TestAcceptanceConsumeMatrix|TestIntegration_CrossRepoMutualDiamond|TestAcceptance_Monorepo' -v` after any bundler/resolver/metadata change.
- Keep diffs minimal and surgical. Prefer updating existing files over creating new ones.
- Do NOT regress batch 1 (items 1, 2, 3) or batch 2 (items 7, 5-part-1) or batch 3:
  - `TestAcceptanceConsumeMatrix`
  - `TestIntegration_CrossRepoMutualDiamond_ConsumesWithTofu`
  - `TestAcceptance_Monorepo_*`
  - `TestAcceptance_Metadata_ProvenanceNonEmptyUnderPlainGoBuild`
  - `TestEffectiveBuildInfoNeverBlankVersion`
  - `TestRunBundleEmbedsNonEmptyProvenanceUnderPlainGoBuild`
  - `TestBundler_AggregationPreservesRootInstallDir`
  - `TestBundler_AggregatePressedSubModules`
  - `TestNamespacedPackageID`
  - `TestEnforceArtifactType`
  - `TestNewFetcherStrictOCIDefault`
  - `TestPlanStrippingModuleDirWarnsOnExcludedRemotePackageSibling`
  - `TestResolveOutPlanFile`

## What is solid (do not break)

- Resolver BFS, cycle detection, boundary enforcement, parallel download + content-hash dedup.
- `resolveSource` tempdir isolation + `//subdir` splitting + git-repo-root boundary expansion (the user's repo is untouched — `git status` clean after runs).
- Single staged bundling pipeline (`stageBundle` → format-only writers) + `repivotMonorepoSources` + `stageMonorepoSiblings` for `//subdir` pivot mode + `aggregatePressedModulesInStaging` (non-destructive).
- Registry→local rewrite drops `version` (item 1). `//subdir` bundles ship the entry at the archive root (item 2). The acceptance matrix gates both (item 3).
- OCI getter core — context propagation, digest verification, single-zip-layer enforcement, strict artifactType (item 10).
- Provenance always non-empty in `meta.json` (item 7).
- Non-destructive aggregation (item 5 part 1): `aggregatePressedModulesInStaging` operates only on the staging tree.
- Namespaced vendor layout (item 6): git-https/registry → `<ns>/<name>`; everything else → `pkg-<sha>`. Dedup by content hash; visible name by address.
- meta.json at `.tofupress/meta.json` (item 6); readers fall back to legacy root for old bundles.
