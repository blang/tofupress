//nolint:gosec // test files use standard permissions and safe paths
package tofupress

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFetcher_FetchHTTPArchive(t *testing.T) {
	// Create a test HTTP server serving a tar.gz archive
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set proper content type for tar.gz
		w.Header().Set("Content-Type", "application/gzip")

		// Create a tar.gz archive on the fly
		gw := gzip.NewWriter(w)
		defer gw.Close() //nolint:errcheck // test cleanup

		tw := tar.NewWriter(gw)
		defer tw.Close() //nolint:errcheck // test cleanup

		// Add a file to the archive
		content := []byte("# Test module\n")
		hdr := &tar.Header{
			Name: "main.tf",
			Mode: 0o644,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	fetcher := NewFetcher()

	// Fetch the archive - use .tar.gz extension so go-getter knows it's an archive
	source := server.URL + "/module.tar.gz"
	err := fetcher.Fetch(context.Background(), tmpDir, source)
	require.NoError(t, err)

	// Verify the file was extracted
	mainTF := filepath.Join(tmpDir, "main.tf")
	assert.FileExists(t, mainTF)

	// Verify content
	data, err := os.ReadFile(mainTF) //nolint:gosec // G304: test file path is safe
	require.NoError(t, err)
	assert.Contains(t, string(data), "Test module")
}

func TestFetcher_FetchGitRepo(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping git test in short mode")
	}

	tmpDir := t.TempDir()
	fetcher := NewFetcher()

	// Fetch a small public git repo with a known structure
	source := "git::https://github.com/hashicorp/terraform.git//website?ref=v1.0.0"
	err := fetcher.Fetch(context.Background(), tmpDir, source)
	require.NoError(t, err)

	// Verify files were extracted
	entries, err := os.ReadDir(tmpDir)
	require.NoError(t, err)
	assert.NotEmpty(t, entries, "expected files to be extracted")
}

func TestFetcher_FetchLocalPath(t *testing.T) {
	// Create a source directory with files
	srcDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "main.tf"), []byte("# local"), 0o644))     //nolint:gosec // G306: test needs standard file permissions
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "variables.tf"), []byte("# vars"), 0o644)) //nolint:gosec // G306: test needs standard file permissions

	// Create destination directory - use a subdirectory to avoid "exists" error
	parentDir := t.TempDir()
	dstDir := filepath.Join(parentDir, "dst")

	fetcher := NewFetcher()
	err := fetcher.Fetch(context.Background(), dstDir, srcDir)
	require.NoError(t, err)

	// Verify files were copied
	assert.FileExists(t, filepath.Join(dstDir, "main.tf"))
	assert.FileExists(t, filepath.Join(dstDir, "variables.tf"))
}

func TestFetcher_FetchWithSubdir(t *testing.T) {
	// Create a test server serving an archive with subdirectories
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")

		gw := gzip.NewWriter(w)
		defer gw.Close() //nolint:errcheck // test cleanup

		tw := tar.NewWriter(gw)
		defer tw.Close() //nolint:errcheck // test cleanup

		// Add a directory
		dirHdr := &tar.Header{
			Name:     "modules/vpc/",
			Mode:     0o755,
			Typeflag: tar.TypeDir,
		}
		if err := tw.WriteHeader(dirHdr); err != nil {
			t.Fatal(err)
		}

		// Add a file in the subdirectory
		content := []byte("# VPC module\n")
		fileHdr := &tar.Header{
			Name: "modules/vpc/main.tf",
			Mode: 0o644,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(fileHdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	fetcher := NewFetcher()

	// Fetch with subdirectory - use .tar.gz extension
	source := server.URL + "/module.tar.gz//modules/vpc"
	err := fetcher.Fetch(context.Background(), tmpDir, source)
	require.NoError(t, err)

	// Verify the file was extracted from the subdirectory
	mainTF := filepath.Join(tmpDir, "main.tf")
	assert.FileExists(t, mainTF)

	data, err := os.ReadFile(mainTF) //nolint:gosec // G304: test file path is safe
	require.NoError(t, err)
	assert.Contains(t, string(data), "VPC module")
}

func TestFetcher_FetchInvalidSource(t *testing.T) {
	tmpDir := t.TempDir()
	fetcher := NewFetcher()

	err := fetcher.Fetch(context.Background(), tmpDir, "invalid::source")
	assert.Error(t, err)
}

func TestFetcher_FetchNonExistentHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	fetcher := NewFetcher()

	err := fetcher.Fetch(context.Background(), tmpDir, server.URL)
	assert.Error(t, err)
}

func TestFetcher_FetchContextCanceled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate slow response
		<-r.Context().Done()
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	fetcher := NewFetcher()

	// Create a canceled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := fetcher.Fetch(ctx, tmpDir, server.URL)
	assert.Error(t, err)
}

func TestFetcher_FetchPreservesDirectoryStructure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")

		gw := gzip.NewWriter(w)
		defer gw.Close() //nolint:errcheck // test cleanup

		tw := tar.NewWriter(gw)
		defer tw.Close() //nolint:errcheck // test cleanup

		// Add nested directories
		dirs := []string{"level1/", "level1/level2/", "level1/level2/level3/"}
		for _, dir := range dirs {
			hdr := &tar.Header{
				Name:     dir,
				Mode:     0o755,
				Typeflag: tar.TypeDir,
			}
			if err := tw.WriteHeader(hdr); err != nil {
				t.Fatal(err)
			}
		}

		// Add file at deepest level
		content := []byte("# deep\n")
		hdr := &tar.Header{
			Name: "level1/level2/level3/deep.tf",
			Mode: 0o644,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	fetcher := NewFetcher()

	// Use .tar.gz extension
	source := server.URL + "/module.tar.gz"
	err := fetcher.Fetch(context.Background(), tmpDir, source)
	require.NoError(t, err)

	// Verify nested structure
	deepFile := filepath.Join(tmpDir, "level1", "level2", "level3", "deep.tf")
	assert.FileExists(t, deepFile)
}

func TestFetcher_FetchEmptyArchive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")

		gw := gzip.NewWriter(w)
		defer gw.Close() //nolint:errcheck // test cleanup

		tw := tar.NewWriter(gw)
		defer tw.Close() //nolint:errcheck // test cleanup
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	fetcher := NewFetcher()

	// Use .tar.gz extension
	source := server.URL + "/empty.tar.gz"
	err := fetcher.Fetch(context.Background(), tmpDir, source)

	// Empty archives should be rejected by go-getter
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty archive")
}

func TestNewFetcher(t *testing.T) {
	fetcher := NewFetcher()
	assert.NotNil(t, fetcher)
}

func TestFetcher_FetchWithQueryParams(t *testing.T) {
	var receivedQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/gzip")

		gw := gzip.NewWriter(w)
		defer gw.Close() //nolint:errcheck // test cleanup
		tw := tar.NewWriter(gw)
		defer tw.Close() //nolint:errcheck // test cleanup

		// Add at least one file to make it a valid archive
		content := []byte("# test\n")
		hdr := &tar.Header{
			Name: "main.tf",
			Mode: 0o644,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	fetcher := NewFetcher()

	// Use .tar.gz extension with query params
	source := server.URL + "/module.tar.gz?version=1.0.0&token=abc123"
	err := fetcher.Fetch(context.Background(), tmpDir, source)
	require.NoError(t, err)

	// Verify query params were preserved
	assert.Contains(t, receivedQuery, "version=1.0.0")
	assert.Contains(t, receivedQuery, "token=abc123")
}
