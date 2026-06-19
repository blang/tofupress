package tofupress

import (
	"context"
	"fmt"

	"github.com/hashicorp/go-getter"
)

// Fetcher downloads remote modules using go-getter.
type Fetcher struct {
	getters   map[string]getter.Getter
	detectors []getter.Detector
}

// NewFetcher creates a new Fetcher with default configuration.
func NewFetcher() *Fetcher {
	return &Fetcher{
		detectors: getter.Detectors,
		getters:   getter.Getters,
	}
}

// Fetch downloads a module from the given source to the destination directory.
// The source can be a git repo, HTTP URL, S3 bucket, GCS bucket, or local path.
// Query parameters in the source are preserved and passed through.
//
// Examples:
//   - git::https://github.com/user/repo.git?ref=v1.0.0
//   - https://example.com/module.tar.gz
//   - s3::https://bucket.s3.amazonaws.com/module.zip
//   - gcs::https://www.googleapis.com/storage/v1/bucket/module.tar.gz
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
