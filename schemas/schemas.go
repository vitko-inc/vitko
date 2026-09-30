// Package schemas embeds the JSON Schemas (2020-12) of every document the
// CLI prints. A schema id such as vitko.runners.estimate/v1 lives at
// schemas/vitko.runners.estimate/v1.json.
package schemas

import (
	"embed"
	"encoding/json"
	"io/fs"
	"sort"
	"strings"
)

//go:embed */*.json
var files embed.FS

// BaseURL prefixes every schema id to form its $id.
const BaseURL = "https://runners.vitko.inc/schemas/"

// IDs lists every schema id, sorted.
func IDs() []string {
	var ids []string
	_ = fs.WalkDir(files, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".json") {
			ids = append(ids, strings.TrimSuffix(p, ".json"))
		}
		return nil
	})
	sort.Strings(ids)
	return ids
}

// Get returns the schema document for id.
func Get(id string) ([]byte, bool) {
	if strings.Contains(id, "..") {
		return nil, false
	}
	b, err := files.ReadFile(id + ".json")
	return b, err == nil
}

// Title returns the schema's title.
func Title(id string) string {
	b, ok := Get(id)
	if !ok {
		return ""
	}
	var s struct {
		Title string `json:"title"`
	}
	_ = json.Unmarshal(b, &s)
	return s.Title
}

// HasField reports whether the dotted path names a property of the schema,
// descending through arrays (items) and nested objects.
func HasField(id, path string) bool {
	b, ok := Get(id)
	if !ok {
		return false
	}
	var node map[string]any
	if json.Unmarshal(b, &node) != nil {
		return false
	}
	for _, seg := range strings.Split(path, ".") {
		node = objectNode(node)
		if node == nil {
			return false
		}
		props, _ := node["properties"].(map[string]any)
		next, ok := props[seg].(map[string]any)
		if !ok {
			return false
		}
		node = next
	}
	return true
}

// objectNode unwraps arrays to their item schema.
func objectNode(n map[string]any) map[string]any {
	for i := 0; i < 4 && n != nil; i++ {
		if _, ok := n["properties"]; ok {
			return n
		}
		items, ok := n["items"].(map[string]any)
		if !ok {
			return nil
		}
		n = items
	}
	return nil
}
