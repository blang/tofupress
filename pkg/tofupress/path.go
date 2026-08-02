package tofupress

import (
	"fmt"
	pathpkg "path"
	"path/filepath"
	"strings"
)

// relativePathEscapesRoot reports whether a filepath.Rel result names the
// parent of its root (or anything below that parent). A plain prefix check is
// incorrect because legal in-root names such as "..cache" also start with two
// dots.
func relativePathEscapesRoot(rel string) bool {
	return rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// joinPathWithin converts an untrusted archive-relative path into a path below
// root. Resolver-generated IDs normally satisfy this already, but Bundler's
// exported API must also reject hand-built trees whose package IDs or module
// keys attempt traversal, absolute placement, or replacement of root itself.
func portableArchivePath(rel string) (string, error) {
	archivePath := filepath.ToSlash(rel)
	if archivePath == "" || strings.ContainsAny(archivePath, "\\:\x00") {
		return "", fmt.Errorf("unsafe portable archive path %q", rel)
	}
	cleaned := pathpkg.Clean(archivePath)
	if cleaned == "." || cleaned == ".." || pathpkg.IsAbs(cleaned) || strings.HasPrefix(cleaned, "../") || cleaned != archivePath {
		return "", fmt.Errorf("unsafe portable archive path %q", rel)
	}
	for _, r := range archivePath {
		if r < ' ' || r == 0x7f {
			return "", fmt.Errorf("unsafe portable archive path %q", rel)
		}
	}
	return archivePath, nil
}

func joinPathWithin(root, rel string) (string, error) {
	cleaned := filepath.Clean(filepath.FromSlash(rel))
	if rel == "" || strings.ContainsAny(rel, "\\:\x00") || cleaned == "." || relativePathEscapesRoot(cleaned) {
		return "", fmt.Errorf("unsafe relative staging path %q", rel)
	}
	target := filepath.Join(root, cleaned)
	check, err := filepath.Rel(root, target)
	if err != nil || check == "." || relativePathEscapesRoot(check) {
		return "", fmt.Errorf("unsafe relative staging path %q", rel)
	}
	return target, nil
}
