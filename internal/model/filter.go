package model

import (
	"path/filepath"
	"strings"
)

// Filter matches records by words or by an absolute folder.
type Filter struct {
	path  string
	words []string
}

// Folder and Terms expose the parsed query to indexed search.
func (f Filter) Folder() string  { return f.path }
func (f Filter) Terms() []string { return f.words }

// ParseFilter reads a search query. "path:" followed by an absolute folder
// matches that folder and everything under it; anything else matches words.
func ParseFilter(query string) Filter {
	query = strings.ToLower(strings.TrimSpace(query))
	if path, ok := strings.CutPrefix(query, "path:"); ok && filepath.IsAbs(strings.TrimSpace(path)) {
		return Filter{path: NormalizedKey(strings.TrimSpace(path))}
	}
	return Filter{words: strings.Fields(query)}
}

// FolderFilter matches one folder and everything under it.
func FolderFilter(path string) Filter {
	return Filter{path: NormalizedKey(path)}
}

// Matches reports whether a record, with optional detail text, matches.
func (f Filter) Matches(item Record, detail string) bool {
	if f.path != "" {
		path := NormalizedKey(item.Path)
		return path == f.path || strings.HasPrefix(path, f.path+`\`)
	}
	if len(f.words) == 0 {
		return true
	}
	haystack := strings.ToLower(item.Name + " " + item.Path + " " + item.Kind + " " + detail)
	for _, term := range f.words {
		if !strings.Contains(haystack, term) {
			return false
		}
	}
	return true
}
