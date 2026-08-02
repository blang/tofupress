package tofupress

import (
	"net/url"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
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
	pkgAddr, subDir := SplitPackageSubdir(raw)
	ref := extractRef(pkgAddr)

	return ModuleSource{
		Raw:         raw,
		Type:        SourceOCI,
		PackageAddr: pkgAddr,
		SubDir:      subDir,
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
	idx := packageSubdirMarker(src)
	if idx == -1 {
		return src, ""
	}

	subDir = src[idx+2:]
	packageAddr = src[:idx]

	// Canonical Terraform addresses put //subdir before the query. Reattach
	// that query to the package address. We also accept query-before-subdir
	// input for compatibility; in that form packageAddr already has the query.
	if queryAt := strings.Index(subDir, "?"); queryAt > -1 {
		packageAddr += subDir[queryAt:]
		subDir = subDir[:queryAt]
	}

	return packageAddr, path.Clean(subDir)
}

// packageSubdirMarker returns the // that separates a package address from its
// subdirectory. It ignores the // in URL schemes. Canonical Terraform syntax
// places the marker before the query string; query-before-subdir is accepted as
// a compatibility form because older host-shorthand normalization emitted it.
func packageSubdirMarker(src string) int {
	queryAt := strings.Index(src, "?")
	canonicalEnd := len(src)
	if queryAt >= 0 {
		canonicalEnd = queryAt
	}

	offset := 0
	if schemeAt := strings.Index(src[:canonicalEnd], "://"); schemeAt >= 0 {
		offset = schemeAt + 3
	}
	if markerAt := strings.Index(src[offset:canonicalEnd], "//"); markerAt >= 0 {
		return offset + markerAt
	}

	// Compatibility form: <package>?ref=...//<subdir>. Use the final
	// non-scheme marker so a nested URL query value's :// is not mistaken for
	// the separator. Canonical syntax remains unambiguous and preferred.
	if queryAt >= 0 {
		for searchEnd := len(src); searchEnd > queryAt+1; {
			rel := strings.LastIndex(src[queryAt+1:searchEnd], "//")
			if rel < 0 {
				break
			}
			markerAt := queryAt + 1 + rel
			if markerAt == 0 || src[markerAt-1] != ':' {
				return markerAt
			}
			searchEnd = markerAt
		}
	}
	return -1
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

	gitURL := "git::https://" + base + ".git"
	if subDir != "" {
		gitURL += "//" + subDir
	}
	return gitURL + query
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

// RedactSourceAddress removes URL user information and sensitive query values
// while preserving enough provenance (getter, host, repository, ref, and
// subdirectory) to identify the dependency. It is intended for logs and
// artifact metadata, never for fetching.
func RedactSourceAddress(raw string) string {
	return escapeSourceDisplayControls(redactSourceAddress(raw))
}

func redactSourceAddress(raw string) string {
	if raw == "" {
		return ""
	}
	packageAddr, subDir := SplitPackageSubdir(raw)

	getterPrefix := ""
	address := packageAddr
	if marker := strings.Index(address, "::"); marker > 0 && !strings.ContainsAny(address[:marker], `/\\`) {
		getterPrefix = address[:marker+2]
		address = address[marker+2:]
	}

	parsed, err := url.Parse(address)
	if err != nil {
		redacted := getterPrefix + redactUnparseableURL(address)
		if subDir == "" {
			return redacted
		}
		if queryAt := strings.Index(redacted, "?"); queryAt >= 0 {
			return redacted[:queryAt] + "//" + subDir + redacted[queryAt:]
		}
		return redacted + "//" + subDir
	}
	parsed.User = nil
	query := parsed.Query()
	for key := range query {
		if isSensitiveQueryKey(key) {
			query.Set(key, "REDACTED")
		}
	}
	parsed.RawQuery = query.Encode()
	redacted := getterPrefix + parsed.String()

	if subDir == "" {
		return redacted
	}
	// Emit the canonical Terraform ordering even if the input used the older
	// query-before-subdir compatibility form.
	if queryAt := strings.Index(redacted, "?"); queryAt >= 0 {
		return redacted[:queryAt] + "//" + subDir + redacted[queryAt:]
	}
	return redacted + "//" + subDir
}

func redactUnparseableURL(address string) string {
	if schemeAt := strings.Index(address, "://"); schemeAt >= 0 {
		authorityStart := schemeAt + 3
		authorityEnd := len(address)
		if separatorAt := strings.IndexAny(address[authorityStart:], "/?#"); separatorAt >= 0 {
			authorityEnd = authorityStart + separatorAt
		}
		authority := address[authorityStart:authorityEnd]
		if userAt := strings.LastIndex(authority, "@"); userAt >= 0 {
			address = address[:authorityStart] + authority[userAt+1:] + address[authorityEnd:]
		}
	}
	if queryAt := strings.Index(address, "?"); queryAt >= 0 {
		fragment := ""
		if fragmentAt := strings.Index(address[queryAt:], "#"); fragmentAt >= 0 {
			fragment = address[queryAt+fragmentAt:]
		}
		address = address[:queryAt] + "?REDACTED" + fragment
	}
	return address
}

func escapeSourceDisplayControls(value string) string {
	if !strings.ContainsFunc(value, unicode.IsControl) {
		return value
	}
	var escaped strings.Builder
	for _, r := range value {
		if unicode.IsControl(r) {
			quoted := strconv.QuoteRune(r)
			escaped.WriteString(strings.Trim(quoted, "'"))
			continue
		}
		escaped.WriteRune(r)
	}
	return escaped.String()
}

func redactSourceSecretsInText(text, raw string) string {
	packageAddr, _ := SplitPackageSubdir(raw)
	for _, address := range sourceAddressVariants(raw, packageAddr) {
		if address != "" {
			text = strings.ReplaceAll(text, address, RedactSourceAddress(address))
		}
	}

	parsed, err := url.Parse(withoutGetterPrefix(packageAddr))
	if err != nil {
		return escapeSourceDisplayControls(text)
	}
	text = redactSourceUserInfo(text, parsed.User)
	return escapeSourceDisplayControls(redactSourceQueryValues(text, parsed.Query()))
}

func sourceAddressVariants(raw, packageAddr string) []string {
	return []string{raw, packageAddr, withoutGetterPrefix(raw), withoutGetterPrefix(packageAddr)}
}

func withoutGetterPrefix(address string) string {
	if marker := strings.Index(address, "::"); marker > 0 && !strings.ContainsAny(address[:marker], `/\\`) {
		return address[marker+2:]
	}
	return address
}

func redactSourceUserInfo(text string, user *url.Userinfo) string {
	if user == nil {
		return text
	}
	if password, ok := user.Password(); ok && password != "" {
		text = strings.ReplaceAll(text, password, "REDACTED")
	}
	username := user.Username()
	for _, userInfo := range []string{user.String(), username, username + ":redacted", username + ":REDACTED"} {
		if userInfo != "" {
			text = strings.ReplaceAll(text, userInfo+"@", "")
		}
	}
	return text
}

func redactSourceQueryValues(text string, query url.Values) string {
	for key, values := range query {
		if !isSensitiveQueryKey(key) {
			continue
		}
		for _, value := range values {
			if value == "" {
				continue
			}
			text = strings.ReplaceAll(text, key+"="+value, key+"=REDACTED")
			text = strings.ReplaceAll(text, url.QueryEscape(key)+"="+url.QueryEscape(value), url.QueryEscape(key)+"=REDACTED")
		}
	}
	return text
}

func isSensitiveQueryKey(key string) bool {
	normalized := strings.ToLower(key)
	normalized = strings.NewReplacer("-", "", "_", "", ".", "").Replace(normalized)
	switch normalized {
	case "sig", "key", "code":
		return true
	}
	for _, marker := range []string{"token", "password", "passwd", "secret", "credential", "signature", "apikey", "privatekey", "authorization"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
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
