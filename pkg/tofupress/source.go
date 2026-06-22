package tofupress

import (
	"net/url"
	"path"
	"path/filepath"
	"strings"
)

// ClassifySource classifies a module source string into a typed ModuleSource.
// The pwd parameter is the directory containing the .tf file (for resolving relative paths).
func ClassifySource(raw, pwd string) ModuleSource {
	// 0. Absolute paths (rejected by default, but classified)
	if IsAbsoluteSource(raw) {
		pkgAddr, subDir := SplitPackageSubdir(raw)
		return ModuleSource{
			Raw:         raw,
			Type:        SourceAbsolute,
			PackageAddr: pkgAddr,
			SubDir:      subDir,
		}
	}

	// 1. Local paths (highest priority)
	if IsLocalSource(raw) {
		pkgAddr, subDir := SplitPackageSubdir(raw)
		return ModuleSource{
			Raw:         raw,
			Type:        SourceLocal,
			PackageAddr: pkgAddr,
			SubDir:      subDir,
		}
	}

	// 2. Explicit prefixed sources
	if strings.HasPrefix(raw, "git::") {
		return parseGitSource(raw)
	}
	if strings.HasPrefix(raw, "s3::") {
		return parseS3Source(raw)
	}
	if strings.HasPrefix(raw, "gcs::") {
		return parseGCSSource(raw)
	}
	if strings.HasPrefix(raw, "oci://") {
		return parseOCISource(raw)
	}

	// 3. HTTP(S) direct URLs
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return parseHTTPSource(raw)
	}

	// 3.5 Host-based shorthand (e.g., github.com/user/repo)
	// Must appear before the registry check because these look like 3-part registry sources.
	if isHostShorthand(raw) {
		return parseGitSource(hostShorthandToGit(raw))
	}

	// 4. Registry module (3-part: namespace/name/provider)
	if isRegistrySource(raw) {
		return parseRegistrySource(raw)
	}

	// 5. Unknown
	return ModuleSource{
		Raw: raw,
	}
}

// parseGitSource parses a git:: prefixed source.
func parseGitSource(raw string) ModuleSource {
	pkgAddr, subDir := SplitPackageSubdir(raw)
	ref := extractRef(pkgAddr)

	return ModuleSource{
		Raw:         raw,
		Type:        SourceGit,
		PackageAddr: pkgAddr,
		SubDir:      subDir,
		Ref:         ref,
	}
}

// parseHTTPSource parses an HTTP(S) source.
func parseHTTPSource(raw string) ModuleSource {
	pkgAddr, subDir := SplitPackageSubdir(raw)

	return ModuleSource{
		Raw:         raw,
		Type:        SourceHTTP,
		PackageAddr: pkgAddr,
		SubDir:      subDir,
	}
}

// parseS3Source parses an s3:: prefixed source.
func parseS3Source(raw string) ModuleSource {
	pkgAddr, subDir := SplitPackageSubdir(raw)

	return ModuleSource{
		Raw:         raw,
		Type:        SourceS3,
		PackageAddr: pkgAddr,
		SubDir:      subDir,
	}
}

// parseGCSSource parses a gcs:: prefixed source.
func parseGCSSource(raw string) ModuleSource {
	pkgAddr, subDir := SplitPackageSubdir(raw)

	return ModuleSource{
		Raw:         raw,
		Type:        SourceGCS,
		PackageAddr: pkgAddr,
		SubDir:      subDir,
	}
}

// parseOCISource parses an oci:// source.
func parseOCISource(raw string) ModuleSource {
	ref := extractRef(raw)

	return ModuleSource{
		Raw:         raw,
		Type:        SourceOCI,
		PackageAddr: raw,
		Ref:         ref,
	}
}

// parseRegistrySource parses a registry module source without making network calls.
// The actual registry API query happens during resolution.
func parseRegistrySource(raw string) ModuleSource {
	// Extract version/ref from query parameters
	ref := extractRef(raw)

	// Strip query parameters before parsing namespace/name/provider
	clean := raw
	if before, _, found := strings.Cut(raw, "?"); found {
		clean = before
	}

	// Parse the registry module format: namespace/name/provider
	parts := strings.Split(clean, "/")
	if len(parts) != 3 {
		return ModuleSource{
			Raw:  raw,
			Type: SourceRegistry,
			Ref:  ref,
		}
	}

	return ModuleSource{
		Raw:               raw,
		Type:              SourceRegistry,
		Ref:               ref,
		RegistryNamespace: parts[0],
		RegistryName:      parts[1],
		RegistryProvider:  parts[2],
	}
}

// SplitPackageSubdir splits a source address into package address and subdirectory.
// The // separator is used to split the address, but // in :// is ignored.
// Query parameters are reattached to the package address.
//
// Examples:
//
//	git::https://github.com/user/repo.git//modules/vpc?ref=v1
//	-> package: git::https://github.com/user/repo.git?ref=v1
//	-> subdir: modules/vpc
func SplitPackageSubdir(src string) (packageAddr, subDir string) {
	// Find ? to know where query params start
	stop := len(src)
	if idx := strings.Index(src, "?"); idx > -1 {
		stop = idx
	}

	// Find // but not in ://
	offset := 0
	if idx := strings.Index(src[:stop], "://"); idx > -1 {
		offset = idx + 3
	}

	// Split at //
	idx := strings.Index(src[offset:stop], "//")
	if idx == -1 {
		return src, ""
	}

	idx += offset
	subdir := src[idx+2:]
	src = src[:idx]

	// Reattach query params to package address
	if idx = strings.Index(subdir, "?"); idx > -1 {
		query := subdir[idx:]
		subdir = subdir[:idx]
		src += query
	}

	return src, path.Clean(subdir)
}

// IsAbsoluteSource returns true if the source is an absolute filesystem path.
func IsAbsoluteSource(raw string) bool {
	return filepath.IsAbs(raw)
}

// IsLocalSource returns true if the source is a local path (starts with ./ or ../).
func IsLocalSource(raw string) bool {
	return strings.HasPrefix(raw, "./") || strings.HasPrefix(raw, "../")
}

// isHostShorthand returns true if raw looks like a host-based git shorthand.
// Examples: github.com/user/repo, bitbucket.org/user/repo
// These look like 3-part registry sources but the first segment is a hostname.
func isHostShorthand(raw string) bool {
	clean := raw

	// Strip query parameters
	if idx := strings.Index(clean, "?"); idx > -1 {
		clean = clean[:idx]
	}

	// Strip //subdir
	if idx := strings.Index(clean, "//"); idx > -1 {
		clean = clean[:idx]
	}

	parts := strings.Split(clean, "/")
	return len(parts) == 3 && strings.Contains(parts[0], ".")
}

// hostShorthandToGit transforms a host-based shorthand (github.com/user/repo)
// into a proper git source URL (git::https://github.com/user/repo.git).
func hostShorthandToGit(raw string) string {
	pkgAddr, subDir := SplitPackageSubdir(raw)

	// Insert .git before query string if present
	base := pkgAddr
	query := ""
	if idx := strings.Index(pkgAddr, "?"); idx > -1 {
		base = pkgAddr[:idx]
		query = pkgAddr[idx:]
	}

	gitURL := "git::https://" + base + ".git" + query

	if subDir != "" {
		gitURL += "//" + subDir
	}

	return gitURL
}

// isRegistrySource returns true if the source looks like a registry module.
// Registry modules have the form: namespace/name/provider (3 parts).
func isRegistrySource(raw string) bool {
	// Remove query parameters
	if idx := strings.Index(raw, "?"); idx > -1 {
		raw = raw[:idx]
	}

	// Skip if it has a prefix or looks like a URL
	if strings.Contains(raw, "::") || strings.Contains(raw, "://") {
		return false
	}

	// Skip local paths
	if strings.HasPrefix(raw, "./") || strings.HasPrefix(raw, "../") || strings.HasPrefix(raw, "/") {
		return false
	}

	// Split by / and check for exactly 3 parts
	parts := strings.Split(raw, "/")
	return len(parts) == 3
}

// extractRef extracts the ref/version/tag from a source string's query parameters.
// It looks for ref, version, or tag parameters.
func extractRef(raw string) string {
	// Parse query parameters
	if _, after, ok := strings.Cut(raw, "?"); ok {
		queryStr := after
		params, err := url.ParseQuery(queryStr)
		if err != nil {
			return ""
		}

		// Look for ref, version, or tag
		if ref := params.Get("ref"); ref != "" {
			return ref
		}
		if version := params.Get("version"); version != "" {
			return version
		}
		if tag := params.Get("tag"); tag != "" {
			return tag
		}
	}

	return ""
}
