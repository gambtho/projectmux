package config

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// lineOf resolves a dotted field path to its 1-based line in one layer's
// document, or 0 when the path names nothing there.
//
// Zero is a real answer, not a failure: a problem about a key the layer
// never set has no position, and reporting a nearby line would point the
// reader at configuration that is not the problem.
func lineOf(root *yaml.Node, path string) int {
	node := nodeAt(root, path)
	if node == nil {
		return 0
	}
	return node.Line
}

// nodeAt walks path and returns the node whose position describes it: the
// key node for a mapping entry, and the element node for a window.
//
// The key rather than the value, because a value that is itself a mapping
// begins on the line after its key — reporting the value would place
// "devcontainer" on its first child.
func nodeAt(root *yaml.Node, path string) *yaml.Node {
	cur := documentBody(root)
	if cur == nil || path == "" {
		return nil
	}

	var found *yaml.Node
	for _, segment := range splitPath(path) {
		if cur == nil {
			return nil
		}
		field, window := parseSegment(segment)
		if field == "" {
			return nil
		}
		if window == "" {
			key := mapEntry(cur, field)
			if key == nil {
				return nil
			}
			found, cur = key, mapValue(cur, field)
			continue
		}
		seq := mapValue(cur, field)
		if seq == nil || seq.Kind != yaml.SequenceNode {
			return nil
		}
		element := windowElement(seq, window)
		if element == nil {
			return nil
		}
		found, cur = element, element
	}
	return found
}

// documentBody unwraps the document node yaml.Unmarshal produces. An empty
// or absent document has no body.
func documentBody(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return nil
		}
		return node.Content[0]
	}
	if node.Kind == 0 {
		return nil
	}
	return node
}

// splitPath divides a path on its separators, ignoring dots inside
// brackets. The portable window charset admits a dot, so "windows[api.v2]"
// is one segment naming one window, not two segments naming nothing.
func splitPath(path string) []string {
	var segments []string
	start, depth := 0, 0
	for i := 0; i < len(path); i++ {
		switch path[i] {
		case '[':
			depth++
		case ']':
			if depth > 0 {
				depth--
			}
		case '.':
			if depth == 0 {
				segments = append(segments, path[start:i])
				start = i + 1
			}
		}
	}
	return append(segments, path[start:])
}

// parseSegment reads "windows[dev]" as the field "windows" and the window
// "dev", and a plain "version" as that field with no window.
func parseSegment(segment string) (field, window string) {
	open := strings.IndexByte(segment, '[')
	if open < 0 || !strings.HasSuffix(segment, "]") {
		return segment, ""
	}
	return segment[:open], segment[open+1 : len(segment)-1]
}

// mapEntry returns the key node for field, or nil when the mapping does not
// carry it.
func mapEntry(node *yaml.Node, field string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == field {
			return node.Content[i]
		}
	}
	return nil
}

// mapValue returns the value node for field.
func mapValue(node *yaml.Node, field string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == field {
			return node.Content[i+1]
		}
	}
	return nil
}

// windowElement finds the sequence element naming window.
//
// The name is matched whole, for the reason mergeWindows gives: a substring
// match would resolve "dev" to a window named "dev-server" and report a
// position the caller never asked about.
func windowElement(seq *yaml.Node, window string) *yaml.Node {
	for _, element := range seq.Content {
		name := mapValue(element, "name")
		if name != nil && name.Value == window {
			return element
		}
	}
	return nil
}
