// Package mapping renders Freight values into a YAML document (a Crossplane claim)
// according to a mapping file. Only the mapped fields are touched; comments and
// key order of the rest of the document are preserved.
package mapping

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

type Mapping struct {
	APIVersion string `yaml:"apiVersion,omitempty"` // expected claim apiVersion (optional check)
	Kind       string `yaml:"kind,omitempty"`       // expected claim kind (optional check)
	Set        []Rule `yaml:"set"`
}

type Rule struct {
	Path  string `yaml:"path"`  // e.g. spec.artifact.ref or metadata.annotations["ship.dev/freight"]
	Value string `yaml:"value"` // text/template over Data
}

// Data is what a mapping can reference. Descriptor fields only: no artifact content.
type Data struct {
	App, Stage string
	Freight    struct {
		Ref, Repository, Digest, Short, ArtifactType string
		Annotations                                  map[string]string
	}
}

func Parse(b []byte) (Mapping, error) {
	var m Mapping
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("mapping: %w", err)
	}
	if len(m.Set) == 0 {
		return m, fmt.Errorf("mapping: set must not be empty")
	}
	for _, r := range m.Set {
		if _, err := splitPath(r.Path); err != nil {
			return m, err
		}
	}
	return m, nil
}

// Render applies the mapping to doc and returns the new document.
func Render(doc []byte, m Mapping, data Data) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(doc, &root); err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("claim: must be a single YAML mapping document")
	}
	top := root.Content[0]
	for field, want := range map[string]string{"apiVersion": m.APIVersion, "kind": m.Kind} {
		if want == "" {
			continue
		}
		if got := lookup(top, field); got == nil || got.Value != want {
			return nil, fmt.Errorf("claim: %s must be %q", field, want)
		}
	}
	for _, r := range m.Set {
		t, err := template.New(r.Path).Option("missingkey=error").Parse(r.Value)
		if err != nil {
			return nil, fmt.Errorf("mapping %s: %w", r.Path, err)
		}
		var v bytes.Buffer
		if err := t.Execute(&v, data); err != nil {
			return nil, fmt.Errorf("mapping %s: %w", r.Path, err)
		}
		segs, _ := splitPath(r.Path)
		if err := set(top, segs, v.String()); err != nil {
			return nil, fmt.Errorf("mapping %s: %w", r.Path, err)
		}
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return nil, err
	}
	return out.Bytes(), enc.Close()
}

func lookup(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func set(n *yaml.Node, segs []string, value string) error {
	for i, s := range segs {
		if n.Kind != yaml.MappingNode {
			return fmt.Errorf("%q is not a mapping", strings.Join(segs[:i], "."))
		}
		child := lookup(n, s)
		last := i == len(segs)-1
		if child == nil {
			child = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			if last {
				child = &yaml.Node{Kind: yaml.ScalarNode}
			}
			n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}, child)
		}
		if last {
			if child.Kind != yaml.ScalarNode {
				return fmt.Errorf("target is not a scalar")
			}
			child.Tag, child.Value, child.Style = "!!str", value, 0
			return nil
		}
		n = child
	}
	return nil
}

// splitPath parses a.b["c/d"].e into [a b c/d e].
func splitPath(p string) ([]string, error) {
	bad := func(why string) ([]string, error) { return nil, fmt.Errorf("mapping path %q: %s", p, why) }
	var segs []string
	needSeg := true // at start and after '.', a segment must follow
	for i := 0; i < len(p); {
		switch {
		case p[i] == '.':
			if needSeg {
				return bad("empty segment")
			}
			needSeg = true
			i++
		case strings.HasPrefix(p[i:], `["`):
			end := strings.Index(p[i+2:], `"]`)
			if end < 0 {
				return bad(`unterminated ["…"]`)
			}
			if end == 0 {
				return bad("empty segment")
			}
			segs = append(segs, p[i+2:i+2+end])
			i += end + 4
			needSeg = false
		default:
			if !needSeg {
				return bad("expected '.' or '[' between segments")
			}
			j := i
			for j < len(p) && p[j] != '.' && p[j] != '[' {
				j++
			}
			segs = append(segs, p[i:j])
			i = j
			needSeg = false
		}
	}
	if len(segs) == 0 || needSeg {
		return bad("empty segment")
	}
	return segs, nil
}
