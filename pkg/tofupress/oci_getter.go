package tofupress

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hashicorp/go-getter"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
)

// OCIGetter implements go-getter's Getter interface for OCI registries.
// It supports the oci:// URL scheme for fetching OpenTofu module packages.
type OCIGetter struct{}

// Get downloads the OCI artifact and decompresses it into dst.
func (g *OCIGetter) Get(dst string, u *url.URL) error {
	ctx := context.Background()

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
	repo.Client = g.authClient(ref.Registry)

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

	// Find a zip layer in the manifest layers
	layerDesc, err := findZipLayer(manifest.Layers)
	if err != nil {
		return fmt.Errorf("oci: %w", err)
	}

	// Fetch and decompress the zip blob
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

// SetClient is a no-op — the OCI getter doesn't need access to the go-getter client.
func (g *OCIGetter) SetClient(c *getter.Client) {}

// findZipLayer finds a layer with zip media type in the manifest layers.
func findZipLayer(layers []ocispec.Descriptor) (ocispec.Descriptor, error) {
	// Media types for zip archives in OCI artifacts
	zipMediaTypes := []string{
		"application/zip",
		"archive/zip",
		"application/vnd.opentofu.modulepkg",
	}

	for _, layer := range layers {
		if slices.Contains(zipMediaTypes, layer.MediaType) {
			return layer, nil
		}
	}

	if len(layers) == 1 {
		// Single layer — assume it's the zip archive regardless of media type
		return layers[0], nil
	}

	if len(layers) == 0 {
		return ocispec.Descriptor{}, fmt.Errorf("no layers found in manifest")
	}

	return ocispec.Descriptor{}, fmt.Errorf("no zip layer found in manifest (available: %v)", layerMediaTypes(layers))
}

func layerMediaTypes(layers []ocispec.Descriptor) []string {
	types := make([]string, len(layers))
	for i, l := range layers {
		types[i] = l.MediaType
	}
	return types
}

// fetchAndDecompress downloads a blob and decompresses it as a zip archive.
func (g *OCIGetter) fetchAndDecompress(ctx context.Context, repo *remote.Repository, desc *ocispec.Descriptor, dst string) error {
	// Fetch the blob content
	rc, err := repo.Fetch(ctx, *desc)
	if err != nil {
		return fmt.Errorf("oci: failed to fetch blob %s: %w", desc.Digest, err)
	}
	defer rc.Close() //nolint:errcheck // best-effort close

	// Write the blob to a temp zip file
	tmpZip, err := os.CreateTemp("", "tofupress-oci-*.zip")
	if err != nil {
		return fmt.Errorf("oci: failed to create temp file: %w", err)
	}
	defer os.Remove(tmpZip.Name()) //nolint:errcheck // best-effort cleanup

	if _, err := io.Copy(tmpZip, rc); err != nil {
		_ = tmpZip.Close()
		return fmt.Errorf("oci: failed to download blob: %w", err)
	}
	if err := tmpZip.Close(); err != nil {
		return fmt.Errorf("oci: failed to finalize download: %w", err)
	}

	// Decompress the zip into dst
	zipDecompressor := &getter.ZipDecompressor{}
	if err := zipDecompressor.Decompress(dst, tmpZip.Name(), true, 0o022); err != nil {
		return fmt.Errorf("oci: failed to decompress artifact: %w", err)
	}

	return nil
}

// authClient creates an authenticated HTTP client for the given registry.
func (g *OCIGetter) authClient(registryHost string) remote.Client {
	cred := g.resolveCredential(registryHost)
	if cred.Username == "" && cred.Password == "" {
		// No credentials found, use default unauthenticated client
		return auth.DefaultClient
	}

	return &auth.Client{
		Client: auth.DefaultClient.Client,
		Header: auth.DefaultClient.Header,
		Cache:  auth.DefaultCache,
		Credential: func(ctx context.Context, hostport string) (auth.Credential, error) {
			return cred, nil
		},
	}
}

// resolveCredential finds credentials for the given registry host.
// Priority: env vars → docker config file.
func (g *OCIGetter) resolveCredential(registryHost string) auth.Credential {
	// 1. Check environment variables
	if user := os.Getenv("OCI_USERNAME"); user != "" {
		return auth.Credential{Username: user, Password: os.Getenv("OCI_PASSWORD")}
	}
	if user := os.Getenv("DOCKER_USERNAME"); user != "" {
		return auth.Credential{Username: user, Password: os.Getenv("DOCKER_PASSWORD")}
	}
	if user := os.Getenv("ORAS_USER"); user != "" {
		return auth.Credential{Username: user, Password: os.Getenv("ORAS_PASS")}
	}

	// 2. Check ~/.docker/config.json
	return g.dockerConfigCredential(registryHost)
}

// dockerConfigCredential reads credentials from ~/.docker/config.json.
func (g *OCIGetter) dockerConfigCredential(registryHost string) auth.Credential {
	configPath := filepath.Join(os.Getenv("HOME"), ".docker", "config.json")
	if envPath := os.Getenv("DOCKER_CONFIG"); envPath != "" {
		configPath = filepath.Join(envPath, "config.json")
	}

	data, err := os.ReadFile(configPath) //nolint:gosec // user's own config file
	if err != nil {
		return auth.EmptyCredential
	}

	var cfg dockerConfig
	//nolint:musttag // simple JSON parsing for auth
	if err := json.Unmarshal(data, &cfg); err != nil {
		return auth.EmptyCredential
	}

	// Try exact host match first, then normalize
	authEntry, ok := cfg.AuthConfigs[registryHost]
	if !ok {
		// Try without scheme prefix
		authEntry, ok = cfg.AuthConfigs["https://"+registryHost]
	}
	if !ok {
		authEntry, ok = cfg.AuthConfigs["http://"+registryHost]
	}
	if !ok {
		return auth.EmptyCredential
	}

	// Decode base64 auth if present (legacy format: "username:password")
	if authEntry.Auth != "" && authEntry.Username == "" {
		decoded, err := base64.StdEncoding.DecodeString(authEntry.Auth)
		if err != nil {
			// Try without padding
			decoded, err = base64.RawStdEncoding.DecodeString(authEntry.Auth)
		}
		if err == nil {
			parts := strings.SplitN(string(decoded), ":", 2)
			if len(parts) == 2 {
				return auth.Credential{Username: parts[0], Password: parts[1]}
			}
		}
	}

	if authEntry.Username != "" {
		return auth.Credential{
			Username:     authEntry.Username,
			Password:     authEntry.Password,
			RefreshToken: authEntry.IdentityToken,
			AccessToken:  authEntry.RegistryToken,
		}
	}

	return auth.EmptyCredential
}

// dockerConfig is a minimal subset of the Docker config file format.
type dockerConfig struct {
	AuthConfigs map[string]dockerAuthEntry `json:"auths"`
}

type dockerAuthEntry struct {
	Username      string `json:"username"`
	Password      string `json:"password"`
	Auth          string `json:"auth"`
	IdentityToken string `json:"identitytoken"`
	RegistryToken string `json:"registrytoken"`
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
