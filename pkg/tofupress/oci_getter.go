package tofupress

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/hashicorp/go-getter"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
)

// modulepkgArtifactType is the artifactType an OCI image manifest MUST carry to be a
// recognized OpenTofu module package (per the OpenTofu OCI module-package spec).
const modulepkgArtifactType = "application/vnd.opentofu.modulepkg"

// zipLayerMediaTypes are the only media types accepted as the module-package archive
// layer. `application/vnd.opentofu.modulepkg` is an *artifact* type, not a layer type,
// and is intentionally NOT accepted here (review finding F7).
var zipLayerMediaTypes = []string{"application/zip", "archive/zip"}

// OCIGetter implements go-getter's Getter interface for OCI registries.
// It supports the oci:// URL scheme for fetching OpenTofu module packages.
type OCIGetter struct {
	client        *getter.Client    // set by SetClient; carries the caller's context (F8)
	roundTripper  http.RoundTripper // optional; injected by NewFetcher(WithRoundTripper(...))
	warningWriter io.Writer         // injected destination for lenient interoperability warnings
	strictOCI     bool              // review item 10: when true (default), reject empty artifactType
}

// SetStrictOCI configures whether the getter enforces the spec's non-empty
// artifactType requirement. Called by NewFetcher(WithStrictOCI(...)) from
// the CLI --strict-oci flag (review item 10).
func (g *OCIGetter) SetStrictOCI(strict bool) { g.strictOCI = strict }

// Lazy-initialized Docker credentials store (handles auths, credsStore, and credHelpers).
var (
	credStore     *credentials.DynamicStore
	errCredStore  error
	credStoreOnce sync.Once
)

func getCredentialsStore() (*credentials.DynamicStore, error) {
	credStoreOnce.Do(func() {
		credStore, errCredStore = credentials.NewStoreFromDocker(credentials.StoreOptions{})
	})
	return credStore, errCredStore
}

// Get downloads the OCI module package and decompresses it into dst.
func (g *OCIGetter) Get(dst string, u *url.URL) error {
	// Propagate the caller's context so cancellation/timeouts interrupt the fetch (F8);
	// fall back to Background when go-getter did not wire a client (synthetic callers).
	ctx := context.Background()
	if g.client != nil && g.client.Ctx != nil {
		ctx = g.client.Ctx
	}

	// Build the OCI reference from the URL
	ref, err := parseOCIRef(u)
	if err != nil {
		return fmt.Errorf("oci: invalid reference: %w", err)
	}

	// Create a remote repository client
	repo, err := remote.NewRepository(ref.String())
	if err != nil {
		return fmt.Errorf("oci: failed to create repository client: %w", err)
	}

	// Configure auth
	repo.Client = g.authClient()

	// Determine the reference (tag or digest) to use
	reference := ref.Reference
	if reference == "" {
		reference = "latest"
	}

	// Fetch the manifest by reference
	_, manifestReader, err := repo.FetchReference(ctx, reference)
	if err != nil {
		return fmt.Errorf("oci: failed to fetch manifest for %s: %w", reference, err)
	}
	defer manifestReader.Close() //nolint:errcheck // best-effort close

	// Parse the manifest to find the zip blob layer
	var manifest ocispec.Manifest
	if decodeErr := json.NewDecoder(manifestReader).Decode(&manifest); decodeErr != nil {
		return fmt.Errorf("oci: failed to decode manifest: %w", decodeErr)
	}

	// Enforce the OpenTofu module-package artifactType (review item 10 / F7).
	// The spec requires artifactType == application/vnd.opentofu.modulepkg exactly.
	// A non-module artifact (e.g. a container image) must be rejected so we never
	// feed a non-zip blob to the zip decompressor. An EMPTY artifactType is NOT a
	// module package per spec: strict mode (default) rejects it; lenient mode
	// (--strict-oci=false) accepts it with a warning, for interop with registries
	// that set the type only on the layer.
	if atErr := enforceArtifactType(g.strictOCI, g.warningWriter, ref.String(), manifest.ArtifactType); atErr != nil {
		return atErr
	}

	// Find the single zip layer in the manifest layers (F7: strict — no blind fallback)
	layerDesc, err := findZipLayer(manifest.Layers)
	if err != nil {
		return fmt.Errorf("oci: %w", err)
	}

	// Fetch, verify the digest, and decompress the zip blob
	return g.fetchAndDecompress(ctx, repo, &layerDesc, dst)
}

// GetFile is not supported for OCI — module packages are always archives.
func (g *OCIGetter) GetFile(dst string, u *url.URL) error {
	return fmt.Errorf("oci: file downloads not supported; OCI module packages are archives")
}

// ClientMode returns ClientModeDir since OCI artifacts are always archives to be extracted.
func (g *OCIGetter) ClientMode(u *url.URL) (getter.ClientMode, error) {
	return getter.ClientModeDir, nil
}

// SetClient stores the go-getter client so Get can propagate the caller's context (F8).
func (g *OCIGetter) SetClient(c *getter.Client) { g.client = c }

// isZipMediaType reports whether a media type denotes an archive/zip layer.
func isZipMediaType(mt string) bool { return slices.Contains(zipLayerMediaTypes, mt) }

// findZipLayer locates the single module-package archive layer (F7). It requires exactly
// one layer with an archive/zip media type; the legacy "single layer → assume zip"
// blind fallback and the `application/vnd.opentofu.modulepkg` layer type are removed so
// a non-zip (or ambiguous) manifest is rejected instead of silently decompressed.
func findZipLayer(layers []ocispec.Descriptor) (ocispec.Descriptor, error) {
	if len(layers) == 0 {
		return ocispec.Descriptor{}, fmt.Errorf("no layers found in manifest")
	}
	var found ocispec.Descriptor
	seen := false
	for i := range layers {
		if !isZipMediaType(layers[i].MediaType) {
			continue
		}
		if seen {
			return ocispec.Descriptor{}, fmt.Errorf("multiple zip layers in manifest — ambiguous; expected exactly one (available: %v)", layerMediaTypes(layers))
		}
		found = layers[i]
		seen = true
	}
	if !seen {
		return ocispec.Descriptor{}, fmt.Errorf("no archive/zip layer found in manifest (available: %v)", layerMediaTypes(layers))
	}
	return found, nil
}

func layerMediaTypes(layers []ocispec.Descriptor) []string {
	types := make([]string, len(layers))
	for i, l := range layers {
		types[i] = l.MediaType
	}
	return types
}

// fetchAndDecompress downloads a blob, verifies its digest, and decompresses it as a zip archive.
func (g *OCIGetter) fetchAndDecompress(ctx context.Context, repo *remote.Repository, desc *ocispec.Descriptor, dst string) error {
	// Fetch the blob content
	rc, err := repo.Fetch(ctx, *desc)
	if err != nil {
		return fmt.Errorf("oci: failed to fetch blob %s: %w", desc.Digest, err)
	}
	defer rc.Close() //nolint:errcheck // best-effort close

	// Write the blob to a temp zip file while hashing it (F8: digest verification)
	tmpZip, err := os.CreateTemp("", "tofupress-oci-*.zip")
	if err != nil {
		return fmt.Errorf("oci: failed to create temp file: %w", err)
	}
	defer os.Remove(tmpZip.Name()) //nolint:errcheck // best-effort cleanup

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmpZip, h), rc); err != nil {
		_ = tmpZip.Close()
		return fmt.Errorf("oci: failed to download blob: %w", err)
	}
	if err := tmpZip.Close(); err != nil {
		return fmt.Errorf("oci: failed to finalize download: %w", err)
	}

	// Verify the downloaded bytes against the layer descriptor digest (F8). A registry
	// or MITM serving a tampered blob that still matches the manifest must be rejected.
	if err := verifyBlobDigest(hex.EncodeToString(h.Sum(nil)), desc.Digest.String()); err != nil {
		return fmt.Errorf("oci: %w", err)
	}

	// Decompress the zip into dst
	zipDecompressor := &getter.ZipDecompressor{}
	if err := zipDecompressor.Decompress(dst, tmpZip.Name(), true, 0o022); err != nil {
		return fmt.Errorf("oci: failed to decompress artifact: %w", err)
	}

	return nil
}

// verifyBlobDigest checks that the computed sha256 hex matches the expected digest of
// the form "sha256:<hex>". An empty expected digest (pre-synthetic tests) is accepted.
func verifyBlobDigest(computedHex, expectedDigest string) error {
	const algo = "sha256:"
	if expectedDigest == "" {
		return nil
	}
	if !strings.HasPrefix(expectedDigest, algo) {
		return fmt.Errorf("unsupported digest algorithm %q (only sha256 is supported)", expectedDigest)
	}
	if want := strings.TrimPrefix(expectedDigest, algo); want != computedHex {
		return fmt.Errorf("blob digest mismatch: downloaded sha256:%s, manifest expects %s", computedHex, expectedDigest)
	}
	return nil
}

// enforceArtifactType validates the manifest's artifactType against the
// OpenTofu module-package spec (review item 10 / F7). Strict (default) rejects
// any artifact whose artifactType is not exactly application/vnd.opentofu.modulepkg
// — including an empty one. Lenient (strict=false) accepts a non-matching
// artifactType with a stderr warning, for interop with registries that set the
// type only on the layer. Returns an error only in strict mode.
func enforceArtifactType(strict bool, warningWriter io.Writer, ref, artifactType string) error {
	if artifactType == modulepkgArtifactType {
		return nil
	}
	safeRef := redactOCIReference(ref)
	if strict {
		return fmt.Errorf("oci: artifact %s is not an OpenTofu module package (artifactType=%q, want %q)",
			safeRef, artifactType, modulepkgArtifactType)
	}
	if warningWriter == nil {
		return nil
	}
	if artifactType == "" {
		fmt.Fprintf(warningWriter, "warning: oci artifact %s has empty artifactType; accepted in lenient mode (--strict-oci=false); spec requires %q\n", safeRef, modulepkgArtifactType) //nolint:errcheck,lll // warning writes are best-effort
	} else {
		fmt.Fprintf(warningWriter, "warning: oci artifact %s has unexpected artifactType=%q; accepted in lenient mode (--strict-oci=false)\n", safeRef, artifactType) //nolint:errcheck,lll // warning writes are best-effort
	}
	return nil
}

func redactOCIReference(ref string) string {
	return strings.TrimPrefix(RedactSourceAddress("oci://"+ref), "oci://")
}

// authClient creates an authenticated HTTP client. When a custom RoundTripper
// was injected (review item 5), it overrides the default auth transport so
// library callers and tests can intercept OCI registry traffic.
func (g *OCIGetter) authClient() remote.Client {
	c := &auth.Client{
		Client:     auth.DefaultClient.Client,
		Header:     auth.DefaultClient.Header,
		Cache:      auth.DefaultCache,
		Credential: g.resolveCredential(),
	}
	if g.roundTripper != nil {
		c.Client = &http.Client{Transport: g.roundTripper}
	}
	return c
}

// resolveCredential returns a credential callback that resolves credentials.
// Priority: env vars → Docker credentials store (handles auths, credsStore, credHelpers).
func (g *OCIGetter) resolveCredential() auth.CredentialFunc {
	return func(ctx context.Context, hostport string) (auth.Credential, error) {
		// 1. Check environment variables (highest priority)
		if user := os.Getenv("OCI_USERNAME"); user != "" {
			return auth.Credential{Username: user, Password: os.Getenv("OCI_PASSWORD")}, nil
		}
		if user := os.Getenv("DOCKER_USERNAME"); user != "" {
			return auth.Credential{Username: user, Password: os.Getenv("DOCKER_PASSWORD")}, nil
		}
		if user := os.Getenv("ORAS_USER"); user != "" {
			return auth.Credential{Username: user, Password: os.Getenv("ORAS_PASS")}, nil
		}

		// 2. Use Docker credentials store (supports auths, credsStore, and credHelpers)
		store, err := getCredentialsStore()
		if err != nil {
			return auth.Credential{}, err
		}
		return store.Get(ctx, hostport)
	}
}

// parseOCIRef parses an oci:// URL into a registry.Reference.
// Expected formats:
//
//	oci://registry.example.com/repo?tag=v1
//	oci://registry.example.com/repo?digest=sha256:abc
//	oci://registry.example.com/repo (defaults to latest)
func parseOCIRef(u *url.URL) (registry.Reference, error) {
	// Reconstruct the reference string: registry/repo:tag
	refStr := u.Host + u.Path

	// Extract tag or digest from query parameters
	tag := u.Query().Get("tag")
	digest := u.Query().Get("digest")

	if tag != "" {
		refStr += ":" + tag
	} else if digest != "" {
		refStr += "@" + digest
	}
	// No tag or digest — let oras-go default to "latest"

	ref, err := registry.ParseReference(refStr)
	if err != nil {
		return registry.Reference{}, fmt.Errorf("failed to parse OCI reference %q: %w", refStr, err)
	}

	return ref, nil
}

// ociDescriptor aliases the OCI descriptor type used by oras-go.
type ociDescriptor = ocispec.Descriptor
