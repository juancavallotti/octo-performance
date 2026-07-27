package render

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// segment is one step of a node path: a mapping key, optionally indexing into the
// sequence it names.
type segment struct {
	key     string
	indexed bool
	index   int // -1 for [*]
}

// parsePath reads "flows[*].source.settings.listeners" into segments.
func parsePath(p string) ([]segment, error) {
	if strings.TrimSpace(p) == "" {
		return nil, fmt.Errorf("empty path")
	}
	var out []segment
	for _, raw := range strings.Split(p, ".") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return nil, fmt.Errorf("path %q has an empty segment", p)
		}
		s := segment{index: -1}
		if open := strings.IndexByte(raw, '['); open >= 0 {
			if !strings.HasSuffix(raw, "]") {
				return nil, fmt.Errorf("path %q: unterminated index in %q", p, raw)
			}
			inner := raw[open+1 : len(raw)-1]
			s.key = raw[:open]
			s.indexed = true
			if inner != "*" {
				n, err := strconv.Atoi(inner)
				if err != nil {
					return nil, fmt.Errorf("path %q: index %q is neither a number nor *", p, inner)
				}
				s.index = n
			}
		} else {
			s.key = raw
		}
		if s.key == "" {
			return nil, fmt.Errorf("path %q has a segment with no key", p)
		}
		out = append(out, s)
	}
	return out, nil
}

// match is one located key/value pair inside a mapping node.
type match struct {
	parent *yaml.Node // the mapping containing the key
	keyIdx int        // index of the key node within parent.Content
	path   string     // the concrete path, e.g. flows[0].workers
	line   int        // source line of the key
}

func (m match) value() *yaml.Node { return m.parent.Content[m.keyIdx+1] }

// findMatches walks node according to segs, collecting every terminal key it reaches.
func findMatches(node *yaml.Node, segs []segment, prefix string, out *[]match) {
	if len(segs) == 0 || node == nil {
		return
	}
	node = deref(node)
	if node.Kind != yaml.MappingNode {
		return
	}
	seg := segs[0]

	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i], node.Content[i+1]
		if key.Value != seg.key {
			continue
		}
		path := seg.key
		if prefix != "" {
			path = prefix + "." + seg.key
		}

		if !seg.indexed {
			if len(segs) == 1 {
				*out = append(*out, match{parent: node, keyIdx: i, path: path, line: key.Line})
				continue
			}
			findMatches(val, segs[1:], path, out)
			continue
		}

		seq := deref(val)
		if seq.Kind != yaml.SequenceNode {
			continue
		}
		for idx, el := range seq.Content {
			if seg.index >= 0 && idx != seg.index {
				continue
			}
			// An indexed segment always descends: a path may not terminate on a
			// sequence element, only on a key inside one.
			if len(segs) > 1 {
				findMatches(el, segs[1:], fmt.Sprintf("%s[%d]", path, idx), out)
			}
		}
	}
}

// deref unwraps document and alias nodes.
func deref(n *yaml.Node) *yaml.Node {
	for n != nil {
		switch n.Kind {
		case yaml.DocumentNode:
			if len(n.Content) == 0 {
				return n
			}
			n = n.Content[0]
		case yaml.AliasNode:
			if n.Alias == nil {
				return n
			}
			n = n.Alias
		default:
			return n
		}
	}
	return n
}

// keyPathsNamed finds every mapping key with the given name, anywhere in the document.
//
// This is what verification uses: a baseline is only honest if the name appears
// nowhere, and a knob that turns up outside its declared path is a scenario bug worth
// failing on rather than a value to leave behind.
func keyPathsNamed(node *yaml.Node, name, prefix string, out *[]string) {
	node = deref(node)
	switch node.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, val := node.Content[i], node.Content[i+1]
			path := key.Value
			if prefix != "" {
				path = prefix + "." + key.Value
			}
			if key.Value == name {
				*out = append(*out, path)
			}
			keyPathsNamed(val, name, path, out)
		}
	case yaml.SequenceNode:
		for i, el := range node.Content {
			keyPathsNamed(el, name, fmt.Sprintf("%s[%d]", prefix, i), out)
		}
	}
}
