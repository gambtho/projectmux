package config

import (
	"cmp"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Origin is a location in a configuration layer.
//
// Line is 0 when the position is not known, which is a real answer rather
// than a missing one: it means the layer set the field in a way the document
// tree cannot point at.
type Origin struct {
	File string
	Line int
}

// originOf builds the origin of path within this layer.
func (s Source) originOf(path string) Origin {
	return Origin{File: s.File, Line: lineOf(s.root, path)}
}

// Merged is the accumulated layer plus the file and line that last set each
// field path. Origins are keyed by the same dotted paths lineOf resolves.
type Merged struct {
	Layer Layer
	// root is the configuration root, used to render positions relative to
	// it so reports read the same on every machine.
	root    string
	origins map[string]Origin
	// files are the layer paths in merge order, which is the order secondary
	// positions are reported in.
	files []string
}

// problem builds a problem attributed to field, optionally naming further
// fields that contributed to it.
//
// Origins[0] is the position of field — the primary, the one a renderer puts
// in front of the message. The rest follow in layer order and then by line,
// so output is stable across runs and diffable across edits.
func (m Merged) problem(field, message string, also ...string) Problem {
	p := Problem{Field: field, Message: message}

	var origins []Origin
	if primary, ok := m.originFor(field); ok {
		origins = append(origins, primary)
	}

	var secondary []Origin
	for _, f := range also {
		o, ok := m.originFor(f)
		if !ok || slices.Contains(origins, o) || slices.Contains(secondary, o) {
			continue
		}
		secondary = append(secondary, o)
	}
	slices.SortStableFunc(secondary, func(a, b Origin) int {
		if c := cmp.Compare(m.layerIndex(a.File), m.layerIndex(b.File)); c != 0 {
			return c
		}
		return cmp.Compare(a.Line, b.Line)
	})

	p.Origins = m.relative(append(origins, secondary...))
	return p
}

// originFor resolves a field to a position through the fallback ladder:
// the field's own node, then the window enclosing it, then nothing. Each
// rung is a weaker but still true statement; no rung invents a position.
func (m Merged) originFor(field string) (Origin, bool) {
	if field == "" {
		return Origin{}, false
	}
	if o, ok := m.origins[field]; ok {
		return o, true
	}
	if window, ok := enclosingWindow(field); ok {
		if o, ok := m.origins[window]; ok {
			return o, true
		}
	}
	return Origin{}, false
}

// enclosingWindow reduces "windows[dev].location" to "windows[dev]".
func enclosingWindow(field string) (string, bool) {
	if !strings.HasPrefix(field, "windows[") {
		return "", false
	}
	end := strings.LastIndexByte(field, ']')
	if end < 0 || end == len(field)-1 {
		return "", false
	}
	return field[:end+1], true
}

// layerIndex orders positions by the layer they came from. An unknown file
// sorts last rather than first, so an unattributed position never displaces
// one the merge actually recorded.
func (m Merged) layerIndex(file string) int {
	if i := slices.Index(m.files, file); i >= 0 {
		return i
	}
	return len(m.files)
}

// relative rewrites positions against the configuration root so reports read
// the same on every machine. A path outside the root, or a root that is not
// known, is left as it is rather than rendered as a chain of "..".
func (m Merged) relative(origins []Origin) []Origin {
	if m.root == "" {
		return origins
	}
	out := make([]Origin, 0, len(origins))
	for _, o := range origins {
		if rel, err := filepath.Rel(m.root, o.File); err == nil && !strings.HasPrefix(rel, "..") {
			o.File = rel
		}
		out = append(out, o)
	}
	return out
}

// credit records that over set path. Later layers overwrite earlier ones, so
// the map always names the layer that actually won — the file the reader has
// to edit.
func (m *Merged) credit(over Source, paths ...string) {
	for _, path := range paths {
		m.creditAs(over.originOf(path), path)
	}
}

// creditAs records one position under several paths. It exists for fields
// that merge as a unit: the three mode keys share whichever key the layer
// physically wrote, because the other two have no node to point at and would
// otherwise report the file with no line.
func (m *Merged) creditAs(origin Origin, paths ...string) {
	if m.origins == nil {
		m.origins = map[string]Origin{}
	}
	for _, path := range paths {
		m.origins[path] = origin
	}
}

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

// windowLines returns the line of every window element carrying name, in
// document order.
//
// It does not go through lineOf, which resolves a name to the first match:
// a duplicate's whole point is that several elements share one name, and
// reporting one of them would send the reader to a definition that looks
// perfectly correct on its own.
func windowLines(root *yaml.Node, name string) []int {
	body := documentBody(root)
	if body == nil {
		return nil
	}
	seq := mapValue(body, "windows")
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return nil
	}
	var lines []int
	for _, element := range seq.Content {
		if got := mapValue(element, "name"); got != nil && got.Value == name {
			lines = append(lines, element.Line)
		}
	}
	return lines
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
