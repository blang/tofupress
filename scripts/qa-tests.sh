#!/usr/bin/env bash
# QA Test Runner for TofuPress
#
# Runs all QA tests against the TofuPress binary and reports pass/fail.
# Uses the fixtures in scripts/qa-fixtures/
#
# Usage:
#   just qa                    # run all tests
#   bash scripts/qa-tests.sh   # run directly
#
# Each test is a function with:
#   - test_name()          # the test function, returns 0 on pass
#   - name must start with "test_"
#   - echo output goes to stderr for progress, stdout for TAP output

set -euo pipefail

# --- Configuration ---
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
FIXTURES_DIR="$PROJECT_DIR/scripts/qa-fixtures"
OUTPUT_DIR="$(mktemp -d)"
BINARY="$OUTPUT_DIR/tofupress"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
NC='\033[0m'

PASS=0
FAIL=0

# --- Helpers ---

build() {
    echo "Building tofupress..." >&2
    (cd "$PROJECT_DIR" && go build -o "$BINARY" ./cmd/tofupress/) || {
        echo "FATAL: Build failed" >&2
        exit 1
    }
}

assert_success() {
    local label="$1"
    local cmd="$2"
    local output
    set +e
    output=$($cmd 2>&1)
    local rc=$?
    set -e
    if [ $rc -eq 0 ]; then
        echo -e "${GREEN}PASS${NC}: $label" >&2
        ((PASS++)) || true
        return 0
    else
        echo -e "${RED}FAIL${NC}: $label (expected success, got rc=$rc)" >&2
        echo "  Command: $cmd" >&2
        echo "  Output: $output" | head -5 >&2
        ((FAIL++)) || true
        return 1
    fi
}

assert_failure() {
    local label="$1"
    local cmd="$2"
    local expect="$3"  # optional expected error substring
    local output
    set +e
    output=$($cmd 2>&1)
    local rc=$?
    set -e
    if [ $rc -ne 0 ]; then
        if [ -n "$expect" ] && ! echo "$output" | grep -qi "$expect"; then
            echo -e "${RED}FAIL${NC}: $label (expected error containing '$expect', got: $(echo "$output" | head -2))" >&2
            ((FAIL++)) || true
            return 1
        fi
        echo -e "${GREEN}PASS${NC}: $label (expected failure)" >&2
        ((PASS++)) || true
        return 0
    else
        echo -e "${RED}FAIL${NC}: $label (expected failure, got success)" >&2
        ((FAIL++)) || true
        return 1
    fi
}

assert_output_contains() {
    local label="$1"
    local cmd="$2"
    local expect="$3"
    local output
    set +e
    output=$($cmd 2>&1)
    local rc=$?
    set -e
    if [ $rc -ne 0 ]; then
        echo -e "${RED}FAIL${NC}: $label (command failed: rc=$rc)" >&2
        echo "  Output: $(echo "$output" | head -3)" >&2
        ((FAIL++)) || true
        return 1
    fi
    if echo "$output" | grep -qi -- "$expect"; then
        echo -e "${GREEN}PASS${NC}: $label" >&2
        ((PASS++)) || true
        return 0
    else
        echo -e "${RED}FAIL${NC}: $label (expected output containing '$expect')" >&2
        echo "  Got: $(echo "$output" | head -3)" >&2
        ((FAIL++)) || true
        return 1
    fi
}

assert_metadata_field() {
    local label="$1"
    local bundle="$2"
    local field="$3"
    local expect="$4"
    local val
    val=$("$BINARY" metadata "$bundle" 2>/dev/null | python3 -c "import sys,json; d=json.load(sys.stdin); print(d$field)" 2>/dev/null || echo "")
    if [ "$val" = "$expect" ]; then
        echo -e "${GREEN}PASS${NC}: $label" >&2
        ((PASS++)) || true
        return 0
    else
        echo -e "${RED}FAIL${NC}: $label (expected $field='$expect', got '$val')" >&2
        ((FAIL++)) || true
        return 1
    fi
}

# --- Unit Tests Using Fixtures ---

test_resolve_simple_module() {
    assert_output_contains "Resolve: simple local child module" \
        "$BINARY resolve $FIXTURES_DIR/simple-module" \
        "Total modules: 2"
}

test_resolve_empty_dir_warns() {
    assert_output_contains "Resolve: empty directory warns" \
        "$BINARY resolve $FIXTURES_DIR/empty-module" \
        "Warning: root module contains no"
}

test_resolve_missing_source_error() {
    assert_failure "Resolve: missing source attribute errors" \
        "$BINARY resolve $FIXTURES_DIR/missing-source" \
        "missing the required.*source"
}

test_resolve_dynamic_source_warns() {
    assert_output_contains "Resolve: dynamic source warns" \
        "$BINARY resolve $FIXTURES_DIR/dynamic-source" \
        "dynamic.*source.*cannot resolve"
}

test_resolve_absolute_source_error() {
    assert_failure "Resolve: absolute source path rejected" \
        "$BINARY resolve $FIXTURES_DIR/absolute-source" \
        "absolute module source"
}

test_resolve_circular_dependency_error() {
    assert_failure "Resolve: circular dependency detected" \
        "$BINARY resolve $FIXTURES_DIR/circular-root" \
        "circular dependency detected"
}

test_resolve_self_ref_error() {
    assert_failure "Resolve: self-reference detected" \
        "$BINARY resolve $FIXTURES_DIR/self-ref" \
        "circular dependency"
}

test_resolve_deep_nested() {
    assert_output_contains "Resolve: 3-level deep nested modules" \
        "$BINARY resolve $FIXTURES_DIR/deep-nested" \
        "Total modules: 4"
}

test_resolve_multi_child() {
    assert_output_contains "Resolve: multiple child modules" \
        "$BINARY resolve $FIXTURES_DIR/multi-child" \
        "Total modules: 3"
}

test_resolve_complex_hcl() {
    assert_output_contains "Resolve: HCL with for_each and count" \
        "$BINARY resolve $FIXTURES_DIR/complex-hcl" \
        "Total modules: 3"
}

test_resolve_subdir_syntax() {
    assert_output_contains "Resolve: //subdir syntax" \
        "$BINARY resolve $FIXTURES_DIR/subdir-test" \
        "Total modules: 2"
}

test_resolve_tofu_priority() {
    # When both .tf and .tofu exist, .tofu wins
    assert_output_contains "Resolve: .tofu priority over .tf" \
        "$BINARY resolve $FIXTURES_DIR/tofu-priority" \
        "Total modules: 1"
}

test_resolve_tofu_module_priority() {
    assert_output_contains "Resolve: .tofu module priority" \
        "$BINARY resolve $FIXTURES_DIR/tofu-module-prio" \
        "tofu_version"
}

test_resolve_utf8_module_name() {
    assert_output_contains "Resolve: UTF-8 module names" \
        "$BINARY resolve $FIXTURES_DIR/utf8-name" \
        "mödulé_αβ"
}

test_resolve_override_file_support() {
    assert_output_contains "Resolve: override .tf files" \
        "$BINARY resolve $FIXTURES_DIR/override-test" \
        "Total modules: 3"
}

test_resolve_terraform_block_coexistence() {
    assert_output_contains "Resolve: terraform {} block coexistence" \
        "$BINARY resolve $FIXTURES_DIR/terraform-block" \
        "Total modules: 2"
}

test_resolve_many_modules() {
    assert_output_contains "Resolve: 10 module blocks" \
        "$BINARY resolve $FIXTURES_DIR/many-modules" \
        "Total modules: 11"
}

test_resolve_json_output() {
    assert_output_contains "Resolve: JSON output format" \
        "$BINARY resolve $FIXTURES_DIR/simple-module --json" \
        '"name": "network"'
}

test_resolve_cross_boundary_error() {
    assert_failure "Resolve: cross-boundary escape blocked" \
        "$BINARY resolve $FIXTURES_DIR/cross-boundary" \
        "escapes package boundary"
}

# --- Adversarial source-shape tests ---

test_resolve_symlink_cycle_error() {
    assert_failure "Resolve: symlink directory cycle rejected" \
        "$BINARY resolve $FIXTURES_DIR/symlink-test" \
        "symlink cycle"
}

test_resolve_heredoc_source() {
    assert_output_contains "Resolve: heredoc source resolved" \
        "$BINARY resolve $FIXTURES_DIR/heredoc" \
        "Total modules: 2"
}

test_resolve_duplicate_names_error() {
    assert_failure "Resolve: duplicate module names rejected" \
        "$BINARY resolve $FIXTURES_DIR/dup-name" \
        "duplicate module"
}

# --- Module/Tree Press Tests ---

test_module_zip_format() {
    local out="$OUTPUT_DIR/test.zip"
    assert_success "Module: ZIP format" \
        "$BINARY module $FIXTURES_DIR/simple-module $out"
    if python3 -c "import zipfile; zipfile.ZipFile('$out'); print('ok')" 2>/dev/null | grep -q ok; then
        echo -e "${GREEN}PASS${NC}: Module: ZIP file is valid" >&2
        ((PASS++)) || true
    else
        echo -e "${RED}FAIL${NC}: Module: ZIP file is not valid" >&2
        ((FAIL++)) || true
    fi
}

test_module_targz_format() {
    local out="$OUTPUT_DIR/test.tar.gz"
    assert_success "Module: tar.gz format" \
        "$BINARY module $FIXTURES_DIR/multi-child $out"
    assert_output_contains "Module: tar.gz has metadata" \
        "$BINARY metadata $out" \
        '"schema_version"'
}

test_module_tarxz_format() {
    local out="$OUTPUT_DIR/test.tar.xz"
    assert_success "Module: tar.xz format" \
        "$BINARY module $FIXTURES_DIR/simple-module $out"
    assert_success "Module: tar.xz metadata readable" \
        "$BINARY metadata $out"
}

test_module_oci_compliant() {
    local out="$OUTPUT_DIR/oci.zip"
    assert_success "Module: OCI-compliant mode" \
        "$BINARY module $FIXTURES_DIR/simple-module $out --oci-compliant"
    assert_metadata_field "Module: OCI metadata oci_compliant=true" \
        "$out" "['command']['options']['oci_compliant']" "True"
}

test_module_oci_requires_zip() {
    assert_failure "Module: OCI-compliant rejects non-zip" \
        "$BINARY module $FIXTURES_DIR/simple-module $OUTPUT_DIR/oci.tar.gz --oci-compliant" \
        "oci-compliant requires zip"
}

test_module_strip_config_only() {
    local out="$OUTPUT_DIR/strip-config.tar.gz"
    assert_success "Module: --strip=config-only" \
        "$BINARY module $FIXTURES_DIR/strip-test $out --strip=config-only"
    assert_metadata_field "Module: canonical strip mode is aggressive" \
        "$out" "['command']['options']['strip_mode']" "aggressive"
    assert_metadata_field "Module: strip alias input is preserved" \
        "$out" "['command']['options']['strip_mode_input']" "config-only"
}

test_module_strip_none() {
    local out="$OUTPUT_DIR/strip-none.tar.gz"
    assert_success "Module: --strip=none" \
        "$BINARY module $FIXTURES_DIR/strip-test $out --strip=none"
}

test_module_strip_bad_value() {
    assert_failure "Module: invalid strip level rejected" \
        "$BINARY module /does/not/exist $OUTPUT_DIR/bad.tar.gz --strip=invalid" \
        "unsupported strip level"
}

test_module_format_bad_value() {
    assert_failure "Module: invalid format rejected" \
        "$BINARY module /does/not/exist $OUTPUT_DIR/bad.bin --format=bad" \
        "unsupported bundle format"
}

test_module_metadata_out() {
    local out="$OUTPUT_DIR/meta-test.tar.gz"
    local meta="$OUTPUT_DIR/meta.json"
    assert_success "Module: --metadata-out writes separate file" \
        "$BINARY module $FIXTURES_DIR/simple-module $out --metadata-out $meta"
    if python3 -c "import json; json.load(open('$meta')); print('ok')" 2>/dev/null | grep -q ok; then
        echo -e "${GREEN}PASS${NC}: Module: metadata file is valid JSON" >&2
        ((PASS++)) || true
    else
        echo -e "${RED}FAIL${NC}: Module: metadata file is not valid JSON" >&2
        ((FAIL++)) || true
    fi
}

test_module_empty_dir_refused() {
    local out="$OUTPUT_DIR/empty.zip"
    assert_failure "Module: empty directory refused" \
        "$BINARY module $FIXTURES_DIR/empty-module $out" \
        "contains no .tf or .tofu files"
}

test_module_idempotent_structure() {
    local out1="$OUTPUT_DIR/idem1.zip"
    local out2="$OUTPUT_DIR/idem2.zip"
    "$BINARY" module "$FIXTURES_DIR/idempotent" "$out1" > /dev/null 2>&1
    "$BINARY" module "$FIXTURES_DIR/idempotent" "$out2" > /dev/null 2>&1
    # Compare ZIP entry structure (names only, excluding embedded metadata).
    local names1 names2
    names1=$(python3 -c "import zipfile; z=zipfile.ZipFile('$out1'); print(' '.join(sorted(n for n in z.namelist() if n!='.tofupress/meta.json')))" 2>/dev/null)
    names2=$(python3 -c "import zipfile; z=zipfile.ZipFile('$out2'); print(' '.join(sorted(n for n in z.namelist() if n!='.tofupress/meta.json')))" 2>/dev/null)
    if [ "$names1" = "$names2" ] && [ -n "$names1" ]; then
        echo -e "${GREEN}PASS${NC}: Module: idempotent entry structure" >&2
        ((PASS++)) || true
    else
        echo -e "${RED}FAIL${NC}: Module: entry structure differs ($names1 vs $names2)" >&2
        ((FAIL++)) || true
    fi
}

test_tree_preserves_subject_shape() {
    local out="$OUTPUT_DIR/tree.zip"
    assert_success "Tree: press succeeds" \
        "$BINARY tree $FIXTURES_DIR/multi-child $out"
    assert_metadata_field "Tree: metadata records tree command" \
        "$out" "['command']['name']" "tree"
}

# --- Metadata Tests ---

test_metadata_read_zip() {
    local out="$OUTPUT_DIR/meta-read.zip"
    "$BINARY" module "$FIXTURES_DIR/simple-module" "$out" > /dev/null 2>&1
    assert_output_contains "Metadata: read from ZIP" \
        "$BINARY metadata $out" \
        '"schema_version"'
}

test_metadata_nonexistent_artifact() {
    assert_failure "Metadata: nonexistent artifact errors" \
        "$BINARY metadata $OUTPUT_DIR/does-not-exist.zip" \
        "not accessible"
}

test_metadata_non_artifact() {
    assert_failure "Metadata: non-artifact file errors" \
        "$BINARY metadata $FIXTURES_DIR/simple-module/main.tf" \
        "could not infer artifact format"
}

test_metadata_corrupt_zip() {
    local corrupt="$OUTPUT_DIR/corrupt.zip"
    echo "not a zip" > "$corrupt"
    assert_failure "Metadata: corrupt zip errors" \
        "$BINARY metadata $corrupt" \
        "not a valid zip"
}

# --- CLI Tests ---

test_cli_help() {
    assert_output_contains "CLI: --help works" \
        "$BINARY --help" \
        "Available Commands"
}

test_cli_version() {
    assert_output_contains "CLI: version command works" \
        "$BINARY version" \
        "tofupress"
}

test_cli_module_help() {
    assert_output_contains "CLI: module --help shows options" \
        "$BINARY module --help" \
        "format string"
}

test_cli_resolve_help() {
    assert_output_contains "CLI: resolve --help shows options" \
        "$BINARY resolve --help" \
        "Output in JSON"
}

test_cli_missing_args() {
    assert_failure "CLI: module with no args errors" \
        "$BINARY module" \
        "received 0"
}

test_cli_bad_source() {
    assert_failure "CLI: nonexistent source errors" \
        "$BINARY resolve /tmp/does-not-exist-12345" \
        "no such file"
}

test_cli_debug_flag_accepted() {
    # --debug should not crash
    assert_success "CLI: --debug flag accepted" \
        "$BINARY resolve $FIXTURES_DIR/simple-module --debug"
}

# --- Main ---

main() {
    echo "=== TofuPress QA Test Suite ===" >&2
    echo "Fixtures: $FIXTURES_DIR" >&2
    echo "Binary:   $BINARY" >&2
    echo "Output:   $OUTPUT_DIR" >&2
    echo "" >&2

    build

    # Run all test_ functions
    local test_funcs
    test_funcs=$(declare -F | grep 'declare -f test_' | awk '{print $3}')
    local count=0
    for fn in $test_funcs; do
        count=$((count + 1))
        "$fn" || true  # don't abort on individual test failure
    done

    echo "" >&2
    echo "========================================" >&2
    echo -e "Results: ${GREEN}$PASS passed${NC}, ${RED}$FAIL failed${NC}" >&2
    echo "Total: $((PASS + FAIL)) tests ($count functions)" >&2
    echo "Temp output kept at: $OUTPUT_DIR" >&2
    echo "========================================" >&2

    # Clean up output dir only if all passed
    if [ "$FAIL" -eq 0 ]; then
        rm -rf "$OUTPUT_DIR"
    else
        echo "Output dir preserved due to failures: $OUTPUT_DIR" >&2
    fi

    return $FAIL
}

main "$@"
