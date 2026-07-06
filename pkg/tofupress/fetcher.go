package tofupress

import (
	"context"
	"fmt"
	"maps"
	"net/http"

	"github.com/hashicorp/go-getter"
)

// Fetcher downloads remote modules using go-getter.
type Fetcher struct {
	getters      map[string]getter.Getter
	detectors    []getter.Detector
	roundTripper http.RoundTripper // optional; injected into the OCI getter and registry HTTP client
	ociGetter    *OCIGetter        // the OCI getter instance, configured with roundTripper
}

// FetcherOption configures a Fetcher.
type FetcherOption func(*Fetcher)

// WithRoundTripper injects a custom http.RoundTripper used by both the registry
// HTTP client and the OCI getter, so library callers and tests can intercept or
// stub network access (review item 5 / ArchAudit R8).
func WithRoundTripper(rt http.RoundTripper) FetcherOption {
	return func(f *Fetcher) { f.roundTripper = rt }
}

// WithGetters replaces the full getter map, giving the caller full control over
// which schemes are supported and how. Useful for test doubles.
func WithGetters(g map[string]getter.Getter) FetcherOption {
	return func(f *Fetcher) { f.getters = g }
}

// WithDetectors replaces the detector chain.
func WithDetectors(d []getter.Detector) FetcherOption {
	return func(f *Fetcher) { f.detectors = d }
}

// NewFetcher creates a new Fetcher with the given options applied on top of the
// default go-getter getters + the OCI getter.
func NewFetcher(opts ...FetcherOption) *Fetcher {
	ociGetter := &OCIGetter{}
	getters := make(map[string]getter.Getter)
	maps.Copy(getters, getter.Getters)
	getters["oci"] = ociGetter

	f := &Fetcher{
		detectors: getter.Detectors,
		getters:   getters,
		ociGetter: ociGetter,
	}
	for _, opt := range opts {
		opt(f)
	}
	// Apply the round-tripper to the OCI getter now that options are settled.
	ociGetter.roundTripper = f.roundTripper
	return f
}

// RoundTripper returns the fetcher's configured transport, or nil when default.
func (f *Fetcher) RoundTripper() http.RoundTripper { return f.roundTripper }

// Fetch downloads a module from the given source to the destination directory.
// The source can be a git repo, HTTP URL, S3 bucket, GCS bucket, OCI registry, or local path.
// Query parameters in the source are preserved and passed through.
//
// Examples:
//   - git::https://github.com/user/repo.git?ref=v1.0.0
//   - https://example.com/module.tar.gz
//   - s3::https://bucket.s3.amazonaws.com/module.zip
//   - gcs::https://www.googleapis.com/storage/v1/bucket/module.tar.gz
//   - oci://registry.example.com/repo?tag=v1.0.0
//   - /local/path/to/module
func (f *Fetcher) Fetch(ctx context.Context, dst, src string) error {
	client := &getter.Client{
		Ctx:  ctx,
		Src:  src,
		Dst:  dst,
		Mode: getter.ClientModeDir,
	}

	if f.detectors != nil {
		client.Detectors = f.detectors
	}

	if f.getters != nil {
		client.Getters = f.getters
	}

	if err := client.Get(); err != nil {
		return fmt.Errorf("failed to fetch module from %s: %w", src, err)
	}

	return nil
}
