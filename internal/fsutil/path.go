// Package fsutil holds small filesystem helpers shared by the Hub.
package fsutil

import "path/filepath"

// AbsOnly returns p when it is an absolute, already-cleaned path and ""
// otherwise. Operator-supplied paths (env vars, config) go through
// filepath.Clean and then AbsOnly before any read or write, so a relative or
// traversing value is refused instead of resolved against the working
// directory.
func AbsOnly(p string) string {
	if p == "" || !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return ""
	}
	return p
}
