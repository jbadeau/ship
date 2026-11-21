// Package policy implements the fail-closed promotion rules (V1–V7 in docs/design.md).
package policy

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jbadeau/ship/internal/config"
	"github.com/jbadeau/ship/internal/freight"
	"github.com/jbadeau/ship/internal/gitx"
	"github.com/jbadeau/ship/internal/ledger"
)

type Checker struct {
	Cfg  *config.File
	Repo gitx.Repo
	Reg  freight.Registry
	Now  func() time.Time // defaults to time.Now; injectable for tests
}

func (c Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Item is the verdict for one changed path.
type Item struct {
	Path       string              `json:"path"`
	App        string              `json:"app,omitempty"`
	Stage      string              `json:"stage,omitempty"`
	Freight    *freight.Descriptor `json:"freight,omitempty"`
	Violations []string            `json:"violations"`
}

type Report struct {
	Base  string `json:"base"`
	Head  string `json:"head"`
	Items []Item `json:"items"`
}

func (r Report) OK() bool {
	for _, it := range r.Items {
		if len(it.Violations) > 0 {
			return false
		}
	}
	return true
}

// Validate checks every pin changed between base and head. Policy (c.Cfg) must
// have been loaded from base by the caller, never from head.
func (c Checker) Validate(ctx context.Context, base, head string) (Report, error) {
	rep := Report{Base: base, Head: head}
	pins, other, err := ledger.Changes(c.Repo, base, head, c.Cfg.LedgerDir)
	if err != nil {
		return rep, err
	}
	// V7: a promotion changes nothing but pins.
	if len(pins) > 0 {
		for _, o := range other {
			rep.Items = append(rep.Items, Item{Path: o.Path, Violations: []string{
				"V7: a promotion MR may only change " + c.Cfg.LedgerDir + "/**; change policy/CI in a separate MR"}})
		}
	}
	for _, ch := range pins {
		it := Item{Path: ch.Path, App: ch.App, Stage: ch.Stage}
		switch {
		case ch.App == "":
			it.Violations = append(it.Violations, "V1: path must be "+c.Cfg.LedgerDir+"/<app>/<stage>.yaml")
		case ch.Status == "D":
			it.Violations = append(it.Violations, "V1: removing a pin is not a promotion")
		default:
			b, found, err := c.Repo.Show(head, ch.Path)
			if err != nil || !found {
				it.Violations = append(it.Violations, fmt.Sprintf("V2: cannot read pin: %v", err))
				break
			}
			pin, err := ledger.Parse(b)
			if err != nil {
				it.Violations = append(it.Violations, "V2: "+err.Error())
				break
			}
			it.Freight, it.Violations = c.CheckPin(ctx, base, ch.App, ch.Stage, pin)
		}
		rep.Items = append(rep.Items, it)
	}
	return rep, nil
}

// CheckPin applies rules V1–V6 to a proposed pin, judging history at base.
func (c Checker) CheckPin(ctx context.Context, base, app, stage string, pin ledger.Pin) (*freight.Descriptor, []string) {
	var v []string
	a, ok := c.Cfg.Apps[app]
	if !ok {
		return nil, []string{fmt.Sprintf("V1: unknown app %q", app)}
	}
	st, ok := c.Cfg.Stage(stage)
	if !ok {
		return nil, []string{fmt.Sprintf("V1: unknown stage %q", stage)}
	}
	// V2: must be a digest reference.
	at := strings.LastIndex(pin.Freight, "@")
	if at < 0 || !strings.HasPrefix(pin.Freight[at+1:], "sha256:") {
		return nil, []string{"V2: freight must be a digest reference (…@sha256:…), tags are not allowed"}
	}
	// V3: repository must be the app's.
	if repo := pin.Freight[:at]; repo != a.Repository {
		return nil, []string{fmt.Sprintf("V3: freight repository %q is not app %q's repository %q", repo, app, a.Repository)}
	}
	// V4: must exist, and be an allowed artifact type.
	desc, err := c.Reg.Resolve(ctx, pin.Freight)
	if err != nil {
		return nil, []string{"V4: " + err.Error()}
	}
	if len(a.ArtifactTypes) > 0 && !slices.Contains(a.ArtifactTypes, desc.ArtifactType) {
		v = append(v, fmt.Sprintf("V4: artifact type %q is not allowed for app %q (allowed: %s)",
			desc.ArtifactType, app, strings.Join(a.ArtifactTypes, ", ")))
	}
	var refs []freight.Descriptor
	var refsErr error
	if st.Upstream != "" || len(st.RequiredEvidence) > 0 {
		refs, refsErr = c.Reg.Referrers(ctx, desc.Ref)
	}
	// V5: ordered progression. The digest must have been pinned upstream (judged
	// against base history only) AND successfully deployed there: a pin whose
	// deploy failed or was refused does not make the Freight eligible downstream.
	if st.Upstream != "" {
		was, err := ledger.EverPinned(c.Repo, base, c.Cfg.LedgerDir, app, st.Upstream, desc.Digest)
		switch {
		case err != nil:
			v = append(v, "V5: "+err.Error())
		case !was:
			v = append(v, fmt.Sprintf("V5: %s was never pinned in upstream stage %q", desc.Short(), st.Upstream))
		case refsErr != nil:
			v = append(v, "V5: "+refsErr.Error())
		default:
			if msg := c.checkDeployed(refs, desc, st); msg != "" {
				v = append(v, msg)
			}
		}
	}
	// V6: required evidence.
	if len(st.RequiredEvidence) > 0 {
		if refsErr != nil {
			v = append(v, "V6: "+refsErr.Error())
		} else {
			for _, req := range st.RequiredEvidence {
				if !slices.ContainsFunc(refs, func(r freight.Descriptor) bool {
					return freight.Matches(r, req.ArtifactType, req.Annotations)
				}) {
					v = append(v, fmt.Sprintf("V6: missing evidence %s %s", req.ArtifactType, fmtAnn(req.Annotations)))
				}
			}
		}
	}
	return &desc, v
}

// checkDeployed requires deployed/<upstream> evidence and, if the stage sets
// upstreamSoak, that the earliest such deploy is at least that old.
func (c Checker) checkDeployed(refs []freight.Descriptor, desc freight.Descriptor, st config.Stage) string {
	var first time.Time
	found := false
	for _, r := range refs {
		if !freight.Matches(r, freight.EvidenceArtifactType, DeployedEvidence(st.Upstream)) {
			continue
		}
		found = true
		t, err := time.Parse(time.RFC3339, r.Annotations[freight.CreatedAnnotation])
		if err != nil {
			continue
		}
		if first.IsZero() || t.Before(first) {
			first = t
		}
	}
	soak := st.UpstreamSoak.Duration
	switch {
	case !found:
		return fmt.Sprintf("V5: %s was pinned in upstream stage %q but never successfully deployed there (no deployed/%s evidence)",
			desc.Short(), st.Upstream, st.Upstream)
	case soak == 0:
		return ""
	case first.IsZero():
		// Fail closed: without a timestamp the soak cannot be proven.
		return fmt.Sprintf("V5: %s: deployed/%s evidence has no %s timestamp; cannot prove upstreamSoak %s",
			desc.Short(), st.Upstream, freight.CreatedAnnotation, soak)
	}
	if age := c.now().Sub(first); age < soak {
		return fmt.Sprintf("V5: %s has soaked in %q for %s, upstreamSoak requires %s (eligible after %s)",
			desc.Short(), st.Upstream, age.Truncate(time.Minute), soak, first.Add(soak).UTC().Format(time.RFC3339))
	}
	return ""
}

// DeployedEvidence is the annotation set ship deploy records after a successful
// deploy to stage, and that V5 requires for the stage downstream of it.
func DeployedEvidence(stage string) map[string]string {
	return map[string]string{"ship.dev/kind": "deployed", "ship.dev/stage": stage, "ship.dev/result": "pass"}
}

func fmtAnn(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + m[k]
	}
	return "{" + strings.Join(parts, ", ") + "}"
}
