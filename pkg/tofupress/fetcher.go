package tofupress

import (
	"context"
	"fmt"
	"maps"

	"github.com/hashicorp/go-getter"
)

// Fetcher downloads remote modules using go-getter.
type Fetcher struct {
	getters   map[string]getter.Getter
	detectors []getter.Detector
}

// NewFetcher creates a new Fetcher with default configuration.
// Includes the OCI getter for oci:// scheme support.
func NewFetcher() *Fetcher {
	getters := make(map[string]getter.Getter)
	maps.Copy(getters, getter.Getters)
	getters["oci"] = &OCIGetter{}

	return &Fetcher{
		detectors: getter.Detectors,
		getters:   getters,
	}
}

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
