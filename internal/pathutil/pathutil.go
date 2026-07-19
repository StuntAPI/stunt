// Package pathutil provides directory-containment validation for file paths
// resolved from adapter-declared content (rules body.file, collection seeds,
// graphql schemas, grpc descriptors, etc.).
//
// The core trust property of stunt is that community adapters cannot read
// files outside their own directory. ContainedPath enforces this by resolving
// a relative path against a base directory and rejecting any path that
// escapes via traversal (../) or absolute paths that leave the base.
package pathutil

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ContainedPath resolves rel against baseDir and returns the cleaned absolute
// path, but ONLY if the result remains within baseDir. It rejects:
//
//   - traversal escapes (e.g. "../../etc/passwd")
//   - absolute paths that resolve outside baseDir
//   - drive-letter or UNC paths on Windows that escape
//
// If rel is already absolute and falls within baseDir, it is accepted. This
// allows already-resolved paths (e.g. from adapter.resolveContainedPath) to
// pass through.
func ContainedPath(baseDir, rel string) (string, error) {
	full := rel
	if !filepath.IsAbs(rel) {
		full = filepath.Join(baseDir, rel)
	}
	cleanPath := filepath.Clean(full)
	relChecked, err := filepath.Rel(baseDir, cleanPath)
	if err != nil || strings.HasPrefix(relChecked, "..") || filepath.IsAbs(relChecked) {
		return "", fmt.Errorf("pathutil: path %q escapes base directory %q", rel, baseDir)
	}
	return cleanPath, nil
}
