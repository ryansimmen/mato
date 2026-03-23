package graph

import (
	"encoding/json"
	"io"
)

// RenderJSON serializes GraphData to the writer as indented JSON.
func RenderJSON(w io.Writer, data GraphData) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(data)
}
