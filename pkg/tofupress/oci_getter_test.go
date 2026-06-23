package tofupress

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
			name:     "single layer unknown type (falls through to single-layer assumption)",
			layers:   []ociDescriptor{makeDesc("application/octet-stream")},
			wantType: "application/octet-stream",
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
			errCont: "no zip layer",
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
