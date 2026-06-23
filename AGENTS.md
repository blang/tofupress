# AGENTS.md

## Project

TofuPress — a CLI that takes an OpenTofu/Terraform root module, recursively resolves all referenced modules, and bundles everything into a single self-contained artifact (tar.gz or OCI Terraform module). Module: `github.com/blang/tofupress`. Alpha: API may change.

## Commands

```
just dev          # goimports → build → run
just test         # go test -v ./...
just lint         # golangci-lint run (alias: just check)
just lint-fix     # golangci-lint run --fix
just lint-ci      # golangci-lint run --out-format=line-number
just lint-yaml    # yamllint -s .
just lint-all     # run all linters (Go, YAML)
just fix          # goimports + go mod tidy
just build        # cross-compile to dist/ (default linux/amd64)
just build-all    # all platforms (linux/darwin, amd64/arm64)
just prek-install # install git pre-commit hooks
just prek-run     # run all pre-commit hooks on all files
```

## Architecture

- **CLI**: `cmd/tofupress/main.go` → `pkg/cmd/` (Cobra commands)
- **Core**: `pkg/tofupress/` — module resolution, bundling logic (to be implemented)

## Tooling

**All tools must be installed exclusively via `mise`.** Do not use brew, apt, pip, or other package managers. Run `mise install` to set up all tools.

| Tool | Version | Notes |
|------|---------|-------|
| Go | 1.26.4 | go.mod; mise.toml |
| golangci-lint | 2.11.4 | `.golangci.yml` |
| goimports | 0.44.0 | `go:golang.org/x/tools/cmd/goimports` |
| prek | 0.3.8 | `prek.toml` |
| trufflehog | 3.95.6 | secret detection in pre-commit |
| yamllint | 1.38.0 | `.yamllint.yml` |

`mise activate` auto-installs pre-commit hooks via the `enter` hook.

## Linting

**Go**: golangci-lint v2 config (`.golangci.yml`): 30 linters including `modernize`, `gocritic`, `revive`, `gosec`. Excludes `dist/` directory. Complexity thresholds: gocyclo 15, gocognit 20.

**YAML**: yamllint (`.yamllint.yml`, strict mode, 120-column width, excludes `dist/`).

Pre-commit hooks (prek): builtin file checks + local hooks for Go (gofmt, goimports, golangci-lint, go-mod-tidy), YAML (yamllint), and secret detection (trufflehog).

## Build

Version info injected via linker flags (`BuildVersion`, `BuildCommit`, `BuildTime`). Cross-compilation uses `CGO_ENABLED=0`. Output binary: `dist/tofupress-{os}-{arch}`.

## Testing

Tests are co-located (`*_test.go` in same package). Run all: `just test`.

## Conventions

- Logging: `slog` with `devslog` for colored output
- Task runner: `just` (not make)
- Git hooks: `prek` (not pre-commit)
- Tool installation: `mise` only (not brew, apt, pip, or other package managers)

## Gotchas

- **Pre-commit amends files**: prek auto-fixes trailing whitespace and EOF newlines; commits fail on first attempt. Always re-`git add` and re-commit.
- **Linter complexity**: `gocyclo 15`, `gocognit 20` — complex functions will need `//nolint:gocognit,gocyclo`.
- **Flag scope**: Use `.Flags()` for command-specific flags, `.PersistentFlags()` only on root or when inheritance is intended. `PersistentFlags` propagates to subcommands.
