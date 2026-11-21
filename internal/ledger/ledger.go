// Package ledger reads and writes pins (ledger/<app>/<stage>.yaml) and their git history.
package ledger

import (
	"bytes"
	"fmt"
	"path"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jbadeau/ship/internal/gitx"
)

// Pin is the content of a pin file. It is deliberately a single field:
// who/why/when live in the commit and the merge request.
type Pin struct {
	Freight string `yaml:"freight"`
}

func Path(dir, app, stage string) string { return path.Join(dir, app, stage+".yaml") }

// Parse decodes a pin strictly.
func Parse(b []byte) (Pin, error) {
	var p Pin
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return Pin{}, fmt.Errorf("pin: %w", err)
	}
	if p.Freight == "" {
		return Pin{}, fmt.Errorf("pin: freight is required")
	}
	return p, nil
}

func Marshal(p Pin) []byte { return []byte("freight: " + p.Freight + "\n") }

// Digest returns the "sha256:…" part of a pinned reference.
func (p Pin) Digest() string {
	if i := strings.LastIndex(p.Freight, "@"); i >= 0 {
		return p.Freight[i+1:]
	}
	return ""
}

// At reads the pin for app/stage at rev. Returns nil if there is none.
func At(repo gitx.Repo, rev, dir, app, stage string) (*Pin, error) {
	b, found, err := repo.Show(rev, Path(dir, app, stage))
	if err != nil || !found {
		return nil, err
	}
	p, err := Parse(b)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// EverPinned reports whether digest was pinned for app/stage at any commit reachable from rev.
func EverPinned(repo gitx.Repo, rev, dir, app, stage, digest string) (bool, error) {
	p := Path(dir, app, stage)
	out, err := repo.Run("log", "--format=%H", rev, "--", p)
	if err != nil {
		return false, err
	}
	for _, sha := range strings.Fields(out) {
		pin, err := At(repo, sha, dir, app, stage)
		if err != nil {
			continue // a historical malformed pin never counts
		}
		if pin != nil && pin.Digest() == digest {
			return true, nil
		}
	}
	return false, nil
}

// Change is one file changed between two revisions.
type Change struct {
	Status string // A, M, D (git name-status letter)
	Path   string
	App    string // set only for well-formed ledger paths
	Stage  string
}

// Changes lists changed files between base and head, splitting ledger files from the rest.
func Changes(repo gitx.Repo, base, head, dir string) (pins []Change, other []Change, err error) {
	out, err := repo.Run("diff", "--name-status", "--no-renames", base, head)
	if err != nil {
		return nil, nil, err
	}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "\t", 2)
		if len(f) != 2 {
			return nil, nil, fmt.Errorf("unexpected diff line %q", line)
		}
		c := Change{Status: f[0][:1], Path: f[1]}
		if !strings.HasPrefix(c.Path, dir+"/") {
			other = append(other, c)
			continue
		}
		rel := strings.TrimPrefix(c.Path, dir+"/")
		parts := strings.Split(rel, "/")
		if len(parts) == 2 && strings.HasSuffix(parts[1], ".yaml") {
			c.App, c.Stage = parts[0], strings.TrimSuffix(parts[1], ".yaml")
		}
		pins = append(pins, c)
	}
	return pins, other, nil
}
