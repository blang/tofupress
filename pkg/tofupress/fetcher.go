package tofupress

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"sync"

	"github.com/hashicorp/go-getter"
)

// Fetcher downloads remote modules using go-getter.
type Fetcher struct {
	getters       map[string]getter.Getter
	customGetters bool
	detectors     []getter.Detector
	roundTripper  http.RoundTripper // optional; injected into the OCI getter and registry HTTP client
	warningWriter io.Writer         // destination for lenient OCI interoperability warnings
	ociGetter     *OCIGetter        // configuration prototype; each default fetch gets a fresh instance
}

// go-getter mutates every Getter by calling SetClient during Client.Get.
// Arbitrary caller-supplied getter instances cannot be cloned safely, so
// serialize their use even when the same instances are shared by two Fetchers.
// Default getters do not use this lock because they are freshly allocated for
// every Fetch call.
var customGettersMu sync.Mutex

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
	return func(f *Fetcher) {
		f.getters = maps.Clone(g)
		if f.getters == nil {
			f.getters = make(map[string]getter.Getter)
		}
		f.customGetters = true
	}
}

// WithDetectors replaces the detector chain.
func WithDetectors(d []getter.Detector) FetcherOption {
	return func(f *Fetcher) { f.detectors = d }
}

// WithStrictOCI toggles the OCI getter's strict-spec enforcement. Strict (the
// default) rejects an artifact with empty or non-matching artifactType; false
// accepts a non-matching artifact with a warning (--strict-oci=false, review
// item 10).
func WithStrictOCI(strict bool) FetcherOption {
	return func(f *Fetcher) {
		if f.ociGetter != nil {
			f.ociGetter.SetStrictOCI(strict)
		}
	}
}

// WithWarningWriter directs lenient OCI interoperability warnings to w. The
// default is os.Stderr; command adapters should inject Cobra's configured error
// writer so tests, embedders, and redirected invocations retain output control.
func WithWarningWriter(w io.Writer) FetcherOption {
	return func(f *Fetcher) { f.warningWriter = w }
}

// NewFetcher creates a new Fetcher with the given options applied on top of the
// default go-getter getters + the OCI getter.
func NewFetcher(opts ...FetcherOption) *Fetcher {
	ociGetter := &OCIGetter{strictOCI: true} // strict by default (review item 10)
	f := &Fetcher{
		detectors:     getter.Detectors,
		getters:       defaultGetterMap(ociGetter),
		warningWriter: os.Stderr,
		ociGetter:     ociGetter,
	}
	for _, opt := range opts {
		opt(f)
	}
	if f.warningWriter != nil {
		f.warningWriter = &synchronizedWriter{dst: f.warningWriter}
	}
	// Apply transport/output configuration after all options are settled.
	ociGetter.roundTripper = f.roundTripper
	ociGetter.warningWriter = f.warningWriter
	return f
}

func defaultGetterMap(ociGetter *OCIGetter) map[string]getter.Getter {
	httpGetter := &getter.HttpGetter{Netrc: true}
	return map[string]getter.Getter{
		"file":  &getter.FileGetter{Copy: true},
		"git":   new(getter.GitGetter),
		"gcs":   new(getter.GCSGetter),
		"hg":    new(getter.HgGetter),
		"s3":    new(getter.S3Getter),
		"http":  httpGetter,
		"https": httpGetter,
		"oci":   ociGetter,
	}
}

type synchronizedWriter struct {
	mu  sync.Mutex
	dst io.Writer
}

func (w *synchronizedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dst.Write(p)
}

func (f *Fetcher) gettersForFetch() map[string]getter.Getter {
	if f.customGetters {
		return f.getters
	}
	ociGetter := &OCIGetter{
		strictOCI:     f.ociGetter.strictOCI,
		roundTripper:  f.roundTripper,
		warningWriter: f.warningWriter,
	}
	return defaultGetterMap(ociGetter)
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
	if f.customGetters {
		customGettersMu.Lock()
		defer customGettersMu.Unlock()
	}

	client := &getter.Client{
		Ctx:     ctx,
		Src:     src,
		Dst:     dst,
		Mode:    getter.ClientModeDir,
		Getters: f.gettersForFetch(),
	}
	if f.detectors != nil {
		client.Detectors = f.detectors
	}
	if err := client.Get(); err != nil {
		return &fetchError{
			source: RedactSourceAddress(src),
			detail: redactSourceSecretsInText(err.Error(), src),
			cause:  err,
		}
	}
	return nil
}

type fetchError struct {
	source string
	detail string
	cause  error
}

func (e *fetchError) Error() string {
	return fmt.Sprintf("failed to fetch module from %s: %s", e.source, e.detail)
}

func (e *fetchError) Unwrap() error { return e.cause }
