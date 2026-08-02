package tofupress

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVerifyBlobDigest(t *testing.T) {
	payload := []byte("hello oci module package")
	sum := sha256.Sum256(payload)
	computedHex := hex.EncodeToString(sum[:])
	validDigest := "sha256:" + computedHex // OCI digest form is lowercase "sha256:"

	tests := []struct {
		name     string
		computed string
		expected string
		wantErr  bool
		errCont  string
	}{
		{
			name:     "matching digest accepted",
			computed: computedHex,
			expected: validDigest,
		},
		{
			name:     "mismatch rejected (tampered blob)",
			computed: strings.Repeat("0", 64),
			expected: validDigest,
			wantErr:  true,
			errCont:  "blob digest mismatch",
		},
		{
			name:     "non-sha digest algorithm rejected",
			computed: computedHex,
			expected: "sha512:" + computedHex,
			wantErr:  true,
			errCont:  "unsupported digest algorithm",
		},
		{
			name:     "empty expected digest accepted (synthetic)",
			computed: computedHex,
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifyBlobDigest(tt.computed, tt.expected)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errCont != "" {
					assert.Contains(t, err.Error(), tt.errCont)
				}
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestParseOCIRef(t *testing.T) {
	tests := []struct {
		name     string
		urlStr   string
		wantReg  string
		wantRepo string
		wantRef  string
		wantErr  bool
		errCont  string
	}{
		{
			name:     "with tag",
			urlStr:   "oci://registry.example.com/my-repo?tag=v1.0.0",
			wantReg:  "registry.example.com",
			wantRepo: "my-repo",
			wantRef:  "v1.0.0",
		},
		{
			name:     "no tag (defaults to latest)",
			urlStr:   "oci://registry.example.com/my-repo",
			wantReg:  "registry.example.com",
			wantRepo: "my-repo",
			wantRef:  "",
		},
		{
			name:     "with digest",
			urlStr:   "oci://registry.example.com/my-repo?digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			wantReg:  "registry.example.com",
			wantRepo: "my-repo",
			wantRef:  "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
		{
			name:     "with port",
			urlStr:   "oci://localhost:5000/my-repo?tag=latest",
			wantReg:  "localhost:5000",
			wantRepo: "my-repo",
			wantRef:  "latest",
		},
		{
			name:     "nested repository",
			urlStr:   "oci://ghcr.io/org/sub/repo?tag=v1",
			wantReg:  "ghcr.io",
			wantRepo: "org/sub/repo",
			wantRef:  "v1",
		},
		{
			name:     "multiple query params (tag wins)",
			urlStr:   "oci://registry.example.com/repo?tag=v1&digest=sha256:abc",
			wantReg:  "registry.example.com",
			wantRepo: "repo",
			wantRef:  "v1",
		},
		{
			name:    "invalid - no host",
			urlStr:  "oci:///repo?tag=v1",
			wantErr: true,
			errCont: "parse",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.urlStr)
			require.NoError(t, err, "failed to parse URL")

			ref, err := parseOCIRef(u)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errCont != "" {
					assert.Contains(t, err.Error(), tt.errCont)
				}
				return
			}
			require.NoError(t, err)

			assert.Equal(t, tt.wantReg, ref.Registry, "registry")
			assert.Equal(t, tt.wantRepo, ref.Repository, "repository")
			assert.Equal(t, tt.wantRef, ref.Reference, "reference")
		})
	}
}

func TestFindZipLayer(t *testing.T) {
	makeDesc := func(mediaType string) ociDescriptor {
		return ociDescriptor{MediaType: mediaType}
	}

	tests := []struct {
		name     string
		layers   []ociDescriptor
		wantType string
		wantErr  bool
		errCont  string
	}{
		{
			name:     "single zip layer",
			layers:   []ociDescriptor{makeDesc("application/zip")},
			wantType: "application/zip",
		},
		{
			name:     "single archive/zip layer",
			layers:   []ociDescriptor{makeDesc("archive/zip")},
			wantType: "archive/zip",
		},
		{
			name:    "single layer unknown type (strict: rejected, no blind fallback)",
			layers:  []ociDescriptor{makeDesc("application/octet-stream")},
			wantErr: true,
			errCont: "no archive/zip layer",
		},
		{
			name:    "opentofu.modulepkg artifact type is NOT a valid layer media type",
			layers:  []ociDescriptor{makeDesc("application/vnd.opentofu.modulepkg")},
			wantErr: true,
			errCont: "no archive/zip layer",
		},
		{
			name:    "multiple zip layers are ambiguous",
			layers:  []ociDescriptor{makeDesc("application/zip"), makeDesc("archive/zip")},
			wantErr: true,
			errCont: "multiple zip layers",
		},
		{
			name:     "multiple layers with zip",
			layers:   []ociDescriptor{makeDesc("text/plain"), makeDesc("application/zip"), makeDesc("image/png")},
			wantType: "application/zip",
		},
		{
			name:     "multiple layers with archive/zip",
			layers:   []ociDescriptor{makeDesc("text/plain"), makeDesc("archive/zip")},
			wantType: "archive/zip",
		},
		{
			name:    "empty layers",
			layers:  []ociDescriptor{},
			wantErr: true,
			errCont: "no layers",
		},
		{
			name:    "multiple layers no zip",
			layers:  []ociDescriptor{makeDesc("text/plain"), makeDesc("image/png")},
			wantErr: true,
			errCont: "no archive/zip layer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			layer, err := findZipLayer(tt.layers)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errCont != "" {
					assert.Contains(t, err.Error(), tt.errCont)
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantType, layer.MediaType)
		})
	}
}

// TestEnforceArtifactType (review item 10) verifies strict mode (default)
// rejects an artifact with empty or non-matching artifactType, while lenient
// warnings use the injected writer and redact reference credentials.
func TestEnforceArtifactType(t *testing.T) {
	const ref = "registry.example.com/repo:latest"
	var warnings strings.Builder

	// Matching artifact type is accepted in both modes.
	assert.NoError(t, enforceArtifactType(true, &warnings, ref, modulepkgArtifactType))
	assert.NoError(t, enforceArtifactType(false, &warnings, ref, modulepkgArtifactType))

	// Strict rejects empty artifactType (the spec gap the review flags).
	err := enforceArtifactType(true, &warnings, ref, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), modulepkgArtifactType)
	assert.Contains(t, err.Error(), `artifactType=""`, "strict mode must name the empty artifactType in the error")

	// Strict rejects a non-matching artifact type.
	err = enforceArtifactType(true, &warnings, ref, "application/vnd.oci.image.manifest.v1+json")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "application/vnd.oci.image.manifest.v1+json")

	// Lenient accepts mismatches and emits warnings to the configured destination.
	assert.NoError(t, enforceArtifactType(false, &warnings, ref, ""))
	assert.NoError(t, enforceArtifactType(false, &warnings, ref, "application/vnd.other"))
	assert.Contains(t, warnings.String(), "warning: oci artifact "+ref)

	warnings.Reset()
	secretRef := "alice:hunter2@registry.example.com/repo:latest"
	assert.NoError(t, enforceArtifactType(false, &warnings, secretRef, ""))
	assert.NotContains(t, warnings.String(), "alice")
	assert.NotContains(t, warnings.String(), "hunter2")
	assert.Contains(t, warnings.String(), "registry.example.com/repo:latest")
}

// TestNewFetcherStrictOCIDefault (review item 10) verifies the OCI getter
// defaults to strict (NewFetcher constructs strictOCI=true) so a plain
// NewFetcher enforces the spec without the caller opting in.
func TestNewFetcherStrictOCIDefault(t *testing.T) {
	f := NewFetcher()
	og := f.ociGetter
	require.NotNil(t, og)
	// Strict default: empty artifactType must error.
	err := enforceArtifactType(og.strictOCI, og.warningWriter, "reg/repo:tag", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), modulepkgArtifactType)
	// WithStrictOCI(false) flips to lenient.
	f2 := NewFetcher(WithStrictOCI(false), WithWarningWriter(io.Discard))
	assert.NoError(t, enforceArtifactType(f2.ociGetter.strictOCI, f2.ociGetter.warningWriter, "reg/repo:tag", ""))
}
