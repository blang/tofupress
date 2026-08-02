# Partially adapted from https://github.com/crazywolf132/ultimate-gojust

# Use bash with strict error checking
set shell := ["bash", "-uc"]

# Version control
# Automatically detect version information from git
# Falls back to timestamp if not in a git repository
version := `git describe --tags --always 2>/dev/null || echo "dev"`
git_commit := `git rev-parse --short HEAD 2>/dev/null || echo "unknown"`
build_time := `date -u '+%Y-%m-%d_%H:%M:%S'`

# Build flags
# Linker flags for embedding version information
ld_flags := "-s -w \
    -extldflags=-static \
    -X '$(go list -m)/pkg/cmd.BuildVersion=" + version + "' \
    -X '$(go list -m)/pkg/cmd.BuildCommit=" + git_commit + "' \
    -X '$(go list -m)/pkg/cmd.BuildTime=" + build_time + "'"

# Directories
# Project directory structure
root_dir := justfile_directory()
bin_dir := root_dir + "/bin"
dist_dir := root_dir + "/dist"
dist_name := "dist"

default: build

dev: goimports build run

fix:
    just goimports
    just tidy

goimports:
    goimports -w ./

check:
    golangci-lint run

lint: check

lint-fix:
    golangci-lint run --fix

lint-ci:
    golangci-lint run --out-format=line-number

lint-yaml:
    yamllint -s .

lint-all: lint lint-yaml trufflehog

trufflehog:
    mise exec github:trufflesecurity/trufflehog -- trufflehog git file://. --fail --no-update

vet:
    go vet ./...

test:
    go test -v ./...

integration-test:
    go test -count=1 -v -tags=integration ./...

# Run fast acceptance tests (local only, no network)
acceptance:
    go test -tags=integration -short -v -parallel=4 -timeout=10m ./pkg/tofupress/

# Run full acceptance test suite (includes network + OCI)
acceptance-full:
    go test -tags=integration -v -timeout=60m ./pkg/tofupress/

# Run a specific acceptance test category
acceptance-cat cat:
    go test -tags=integration -v -timeout=15m ./pkg/tofupress/ -run 'TestAcceptance_{{cat}}'

tidy:
    go mod tidy

run:
    ./tofupress

build platform="linux/amd64/-":
    #!/usr/bin/env sh
    mkdir -p "{{dist_dir}}"
    platform="{{platform}}"
    echo "Platform: $platform"
    os=$(echo $platform | cut -d/ -f1)
    arch=$(echo $platform | cut -d/ -f2)
    arm=$(echo $platform | cut -d/ -f3)
    output="{{dist_name}}/tofupress-${os}-${arch}"

    CGO_ENABLED="0" GOOS=$os GOARCH=$arch $([ "$arm" != "-" ] && echo "GOARM=$arm") \
    go build \
        -trimpath \
        -ldflags '{{ld_flags}}' \
        -o "$output" \
        ./cmd/tofupress

build-all:
    #!/usr/bin/env sh
    mkdir -p "{{dist_dir}}"
    version="{{version}}"
    for platform in \
        "linux/amd64/-" \
        "linux/arm64/-" \
        "darwin/amd64/-" \
        "darwin/arm64/-"; do
        os=$(echo $platform | cut -d/ -f1)
        arch=$(echo $platform | cut -d/ -f2)
        arm=$(echo $platform | cut -d/ -f3)
        staging="tofupress-${version}-${os}-${arch}"
        staging_dir="{{dist_dir}}/${staging}"

        mkdir -p "$staging_dir"
        CGO_ENABLED=0 GOOS=$os GOARCH=$arch $([ "$arm" != "-" ] && echo "GOARM=$arm") \
        go build \
            -trimpath \
            -ldflags '{{ld_flags}}' \
            -o "$staging_dir/tofupress" \
            ./cmd/tofupress

        tar -C "{{dist_dir}}" -czf "{{dist_dir}}/${staging}.tar.gz" "$staging"
        rm -rf "$staging_dir"
    done

qa:
    bash scripts/qa-tests.sh

prek-install:
    prek install

prek-run:
    prek run --all-files

# Release — auto-detects version from the latest git tag.
# Requires: git tag already created and pushed, gh CLI authenticated.
# Example: git tag v0.2.0 && git push origin v0.2.0 && just release
release:
    #!/usr/bin/env sh
    ./scripts/release.sh
