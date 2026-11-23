// Package config loads and validates ship.yaml, the promotion policy of a ledger repo.
package config

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const APIVersion = "ship/v1alpha1"

// File is the parsed ship.yaml.
type File struct {
	APIVersion string         `yaml:"apiVersion"`
	LedgerDir  string         `yaml:"ledgerDir,omitempty"`
	Apps       map[string]App `yaml:"apps"`
	// DeployTemplates are named, reusable deploy blocks (e.g. "service",
	// "application") so a repo layout is configuration, not code.
	DeployTemplates map[string]Deploy         `yaml:"deployTemplates,omitempty"`
	Stages          []Stage                   `yaml:"stages"`
	Registries      map[string]RegistryConfig `yaml:"registries,omitempty"`
	Forge           ForgeConfig               `yaml:"forge,omitempty"`
	// Deprecated: the pre-adapter name of the forge section. Accepted when forge
	// is not set; setting both is an error.
	LegacyGitLab *ForgeConfig `yaml:"gitlab,omitempty"`
	// Identities maps a requester identity (Kubernetes username, agent id, …) to
	// a forge username, for V8. Unmapped identities are used as-is.
	Identities map[string]string `yaml:"identities,omitempty"`
}

// ForgeUser maps a requester identity to a forge username.
func (f *File) ForgeUser(identity string) string {
	if u, ok := f.Identities[identity]; ok {
		return u
	}
	return identity
}

type App struct {
	Repository    string   `yaml:"repository"`
	ArtifactTypes []string `yaml:"artifactTypes,omitempty"`
	Deploy        Deploy   `yaml:"deploy"`
	// Vars are app-specific placeholder values for driver config, e.g.
	// {group: api-gw} used as ${group}. ${app} and ${stage} are always defined.
	Vars map[string]string `yaml:"vars,omitempty"`
}

// Deploy selects the app's driver. Config is app-wide; Stages holds per-stage
// config merged over it. Both are opaque to ship and interpreted by the driver.
type Deploy struct {
	Template string                    `yaml:"template,omitempty"` // name of a deployTemplates entry
	Driver   string                    `yaml:"driver,omitempty"`
	Config   map[string]any            `yaml:"config,omitempty"`
	Stages   map[string]map[string]any `yaml:"stages,omitempty"`
}

type Stage struct {
	Name             string        `yaml:"name"`
	Upstream         string        `yaml:"upstream,omitempty"`
	RequiredEvidence []EvidenceReq `yaml:"requiredEvidence,omitempty"`
	// UpstreamSoak is the minimum time a Freight must have been running in the
	// upstream stage (since its first deployed/<upstream> evidence) before it may
	// be promoted here. Go duration syntax, e.g. "24h". Requires upstream.
	UpstreamSoak Duration `yaml:"upstreamSoak,omitempty"`
}

// Duration is a time.Duration that unmarshals from Go duration syntax ("90m", "24h").
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	if v < 0 {
		return fmt.Errorf("duration %q must not be negative", s)
	}
	d.Duration = v
	return nil
}

type EvidenceReq struct {
	ArtifactType string            `yaml:"artifactType"`
	Annotations  map[string]string `yaml:"annotations,omitempty"`
}

type RegistryConfig struct {
	Insecure bool `yaml:"insecure,omitempty"`
}

// ForgeConfig names ship's accounts on the forge and the ledger approval rule.
// Field names keep their GitLab-era spelling; "project" means repo on any forge.
type ForgeConfig struct {
	// Type is the forge adapter the ledger expects (default gitlab). Informational
	// for now: the runtime forge is selected by SHIP_FORGE (docs/adapters.md).
	Type               string `yaml:"type,omitempty"`
	LedgerProject      string `yaml:"ledgerProject,omitempty"`
	BotUsername        string `yaml:"botUsername,omitempty"`      // ship-bot: env MRs
	PromoterUsername   string `yaml:"promoterUsername,omitempty"` // ship-promoter: ledger MRs from Promotion CRs
	ApproverUsername   string `yaml:"approverUsername,omitempty"` // ship-approver: env MR approvals
	LedgerMinApprovals int    `yaml:"ledgerMinApprovals,omitempty"`
}

// ServiceAccounts lists ship's own forge accounts; their approvals never count (V8).
func (g ForgeConfig) ServiceAccounts() []string {
	var out []string
	for _, u := range []string{g.BotUsername, g.PromoterUsername, g.ApproverUsername} {
		if u != "" {
			out = append(out, u)
		}
	}
	return out
}

// nameRE allows service ids such as SV-API_GW_MGT and stages such as DEV. Names
// end up in file paths, branch names and GitLab paths, so they stay conservative.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Parse decodes ship.yaml strictly (unknown fields are errors) and validates it.
func Parse(b []byte) (*File, error) {
	var f File
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("ship.yaml: %w", err)
	}
	if f.LedgerDir == "" {
		f.LedgerDir = "ledger"
	}
	if f.LegacyGitLab != nil {
		if f.Forge != (ForgeConfig{}) {
			return nil, fmt.Errorf("ship.yaml: set either forge or gitlab (deprecated), not both")
		}
		f.Forge, f.LegacyGitLab = *f.LegacyGitLab, nil
		if f.Forge.Type == "" {
			f.Forge.Type = "gitlab"
		}
	}
	if f.Forge.Type == "" {
		f.Forge.Type = "gitlab"
	}
	if f.Forge.LedgerMinApprovals == 0 {
		f.Forge.LedgerMinApprovals = 1
	}
	if err := f.applyTemplates(); err != nil {
		return nil, fmt.Errorf("ship.yaml: %w", err)
	}
	if err := f.validate(); err != nil {
		return nil, fmt.Errorf("ship.yaml: %w", err)
	}
	return &f, nil
}

// applyTemplates merges each app's deploy block over its template:
// driver is replaced if set; config and per-stage config are merged key by key.
func (f *File) applyTemplates() error {
	for name, t := range f.DeployTemplates {
		if t.Template != "" {
			return fmt.Errorf("deployTemplates.%s: templates cannot reference templates", name)
		}
	}
	for name, a := range f.Apps {
		if a.Deploy.Template == "" {
			continue
		}
		t, ok := f.DeployTemplates[a.Deploy.Template]
		if !ok {
			return fmt.Errorf("app %q: unknown deploy template %q", name, a.Deploy.Template)
		}
		merged := Deploy{Template: a.Deploy.Template, Driver: t.Driver,
			Config: mergeMaps(t.Config, a.Deploy.Config), Stages: map[string]map[string]any{}}
		if a.Deploy.Driver != "" {
			merged.Driver = a.Deploy.Driver
		}
		for st, c := range t.Stages {
			merged.Stages[st] = mergeMaps(c, nil)
		}
		for st, c := range a.Deploy.Stages {
			merged.Stages[st] = mergeMaps(merged.Stages[st], c)
		}
		a.Deploy = merged
		f.Apps[name] = a
	}
	return nil
}

func mergeMaps(base, over map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

func (f *File) validate() error {
	if f.APIVersion != APIVersion {
		return fmt.Errorf("apiVersion must be %q, got %q", APIVersion, f.APIVersion)
	}
	if len(f.Stages) == 0 {
		return fmt.Errorf("at least one stage is required")
	}
	seen := map[string]bool{}
	for _, s := range f.Stages {
		if !nameRE.MatchString(s.Name) {
			return fmt.Errorf("invalid stage name %q", s.Name)
		}
		if seen[s.Name] {
			return fmt.Errorf("duplicate stage %q", s.Name)
		}
		// Upstream must be declared earlier: guarantees an acyclic graph.
		if s.Upstream != "" && !seen[s.Upstream] {
			return fmt.Errorf("stage %q: upstream %q must be declared before it", s.Name, s.Upstream)
		}
		if s.UpstreamSoak.Duration > 0 && s.Upstream == "" {
			return fmt.Errorf("stage %q: upstreamSoak requires upstream", s.Name)
		}
		for _, e := range s.RequiredEvidence {
			if e.ArtifactType == "" {
				return fmt.Errorf("stage %q: requiredEvidence entry without artifactType", s.Name)
			}
		}
		seen[s.Name] = true
	}
	for name, a := range f.Apps {
		if !nameRE.MatchString(name) {
			return fmt.Errorf("invalid app name %q", name)
		}
		if a.Repository == "" || strings.ContainsAny(a.Repository, "@") || hasTag(a.Repository) {
			return fmt.Errorf("app %q: repository must be a bare repository (no tag or digest), got %q", name, a.Repository)
		}
		if a.Deploy.Driver == "" {
			return fmt.Errorf("app %q: deploy.driver is required", name)
		}
		for st := range a.Deploy.Stages {
			if !seen[st] {
				return fmt.Errorf("app %q: deploy config for unknown stage %q", name, st)
			}
		}
	}
	return nil
}

// hasTag reports whether the last path segment carries a ":tag".
func hasTag(repo string) bool {
	last := repo[strings.LastIndex(repo, "/")+1:]
	return strings.Contains(last, ":")
}

func (f *File) Stage(name string) (Stage, bool) {
	for _, s := range f.Stages {
		if s.Name == name {
			return s, true
		}
	}
	return Stage{}, false
}

// DriverConfig returns the app-wide driver config with the stage's config merged
// over it. In string values, ${app}, ${stage} and the app's vars (${name}) are
// replaced, so one template line such as envRepo: sda/services/${app}/${stage}
// covers every app and stage that follows that layout.
func (f *File) DriverConfig(app, stage string) map[string]any {
	a := f.Apps[app]
	out := mergeMaps(a.Deploy.Config, a.Deploy.Stages[stage])
	pairs := []string{"${app}", app, "${stage}", stage}
	for k, v := range a.Vars {
		pairs = append(pairs, "${"+k+"}", v)
	}
	r := strings.NewReplacer(pairs...)
	for k, v := range out {
		if s, ok := v.(string); ok {
			out[k] = r.Replace(s)
		}
	}
	return out
}

// InsecureRegistries lists registries configured for plain HTTP.
func (f *File) InsecureRegistries() []string {
	var out []string
	for host, r := range f.Registries {
		if r.Insecure {
			out = append(out, host)
		}
	}
	return out
}

// String reads a string value from opaque driver config.
func String(cfg map[string]any, key string) (string, error) {
	v, ok := cfg[key]
	if !ok {
		return "", fmt.Errorf("driver config: %q is required", key)
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", fmt.Errorf("driver config: %q must be a non-empty string", key)
	}
	return s, nil
}

// StringOr reads an optional string value from opaque driver config.
func StringOr(cfg map[string]any, key, def string) string {
	if s, ok := cfg[key].(string); ok && s != "" {
		return s
	}
	return def
}
