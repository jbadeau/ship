// Package secret resolves secrets from references instead of values
// (docs/adapters.md §3). A reference names where a secret lives:
//
//	env://OCTOPUS_API_KEY           an environment variable (CI variables, injected secrets)
//	file:///var/run/secrets/token   a file (k8s Secret, Vault Agent, External Secrets, CSI)
//
// Each secret ship uses has a logical name (see Names) and a default reference
// that matches the environment variable ship has always read. The deployment
// overrides it with SHIP_SECRET_<NAME>=<ref>, e.g.
// SHIP_SECRET_OCTOPUS_API_KEY=file:///var/run/secrets/octopus/api-key.
//
// Secret locations are a property of where ship runs (a CI job, the approver
// pod), not of the ledger, so they are not configured in ship.yaml.
//
// Values are wrapped in Value, which never prints itself. Resolution fails
// closed: a missing secret is an error, never an empty string.
package secret

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Logical secret names and the environment variable each one defaults to.
const (
	ForgeToken         = "forge-token"          // ship-bot / ship-promoter API token
	ForgeApproverToken = "forge-approver-token" // ship-approver API token (approver service only)
	ForgeReadToken     = "forge-read-token"     // read-only API token (env-repo CI)
	WebhookSecret      = "webhook-secret"       // forge webhook shared secret
	OctopusAPIKey      = "octopus-api-key"
)

// Names maps each known secret to its default reference.
var Names = map[string]string{
	ForgeToken:         "env://SHIP_GITLAB_TOKEN",
	ForgeApproverToken: "env://SHIP_APPROVER_TOKEN",
	ForgeReadToken:     "env://SHIP_READ_TOKEN",
	WebhookSecret:      "env://SHIP_WEBHOOK_SECRET",
	OctopusAPIKey:      "env://OCTOPUS_API_KEY",
}

// OverrideVar is the environment variable that overrides name's reference.
func OverrideVar(name string) string {
	return "SHIP_SECRET_" + strings.ToUpper(strings.NewReplacer("-", "_", "/", "_", ".", "_").Replace(name))
}

// ---- Value ----

// Value holds secret bytes. It formats as [redacted] everywhere (fmt verbs,
// JSON, text marshalling); Reveal is the only way to read it.
type Value struct{ b []byte }

func NewValue(s string) Value { return Value{b: []byte(s)} }

func (v Value) Reveal() string { return string(v.b) }
func (v Value) Empty() bool    { return len(v.b) == 0 }

const redacted = "[redacted]"

func (Value) String() string               { return redacted }
func (Value) GoString() string             { return redacted }
func (Value) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(redacted)) }
func (Value) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }
func (Value) MarshalText() ([]byte, error) { return []byte(redacted), nil }
func (v Value) Equal(o Value) bool         { return string(v.b) == string(o.b) }
func (Value) LogValue() slog.Value         { return slog.StringValue(redacted) }

// Secret is a resolved value plus its expiry (zero = static).
type Secret struct {
	Value   Value
	Expires time.Time
}

// ---- references ----

// Ref is a parsed reference: scheme://path[#key].
type Ref struct {
	Scheme, Path, Key string
}

func (r Ref) String() string {
	s := r.Scheme + "://" + r.Path
	if r.Key != "" {
		s += "#" + r.Key
	}
	return s
}

func ParseRef(s string) (Ref, error) {
	scheme, rest, ok := strings.Cut(strings.TrimSpace(s), "://")
	if !ok || scheme == "" || rest == "" {
		return Ref{}, fmt.Errorf("secret reference %q: want scheme://path, e.g. env://NAME or file:///path", s)
	}
	path, key, _ := strings.Cut(rest, "#")
	return Ref{Scheme: scheme, Path: path, Key: key}, nil
}

// ---- errors ----

// ErrNotFound means the reference resolved to nothing (unset variable, missing file).
var ErrNotFound = errors.New("secret not found")

// IsNotFound reports whether err is (or wraps) ErrNotFound.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// ---- providers ----

// Provider resolves references of one scheme.
type Provider interface {
	Scheme() string
	Get(ctx context.Context, ref Ref) (Secret, error)
}

// Env reads environment variables.
type Env struct {
	Lookup func(string) (string, bool) // default os.LookupEnv
}

func (Env) Scheme() string { return "env" }

func (e Env) Get(_ context.Context, ref Ref) (Secret, error) {
	if ref.Key != "" {
		return Secret{}, fmt.Errorf("%s: env references take no #key", ref)
	}
	look := e.Lookup
	if look == nil {
		look = os.LookupEnv
	}
	v, ok := look(ref.Path)
	if !ok || v == "" {
		return Secret{}, fmt.Errorf("%s: %w", ref, ErrNotFound)
	}
	return Secret{Value: NewValue(v)}, nil
}

// File reads a file on every Get, so rotated files (Kubernetes Secret volumes,
// Vault Agent templates) take effect without a restart. One trailing newline is
// trimmed.
type File struct{}

func (File) Scheme() string { return "file" }

func (File) Get(_ context.Context, ref Ref) (Secret, error) {
	if ref.Key != "" {
		return Secret{}, fmt.Errorf("%s: file references take no #key", ref)
	}
	b, err := os.ReadFile(ref.Path)
	if errors.Is(err, os.ErrNotExist) {
		return Secret{}, fmt.Errorf("%s: %w", ref, ErrNotFound)
	}
	if err != nil {
		return Secret{}, fmt.Errorf("%s: %w", ref, err)
	}
	s := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	if s == "" {
		return Secret{}, fmt.Errorf("%s: file is empty: %w", ref, ErrNotFound)
	}
	return Secret{Value: NewValue(s)}, nil
}

// ---- resolver ----

// Resolver maps logical names to references and resolves them through providers.
// Secrets with an expiry are cached until shortly before it; static ones are
// read on every call (env and file reads are cheap, and this picks up rotation).
type Resolver struct {
	providers map[string]Provider
	refs      map[string]Ref
	now       func() time.Time

	mu    sync.Mutex
	cache map[string]Secret
}

// Margin is how long before expiry a cached secret is refreshed.
const Margin = 30 * time.Second

// NewResolver builds a resolver with the env and file providers, the default
// references in Names, and SHIP_SECRET_<NAME> overrides from lookup (os.LookupEnv
// when nil). Extra providers (vault, exec, …) are added with Register.
func NewResolver(lookup func(string) (string, bool)) (*Resolver, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	r := &Resolver{providers: map[string]Provider{}, refs: map[string]Ref{}, now: time.Now, cache: map[string]Secret{}}
	r.Register(Env{Lookup: lookup})
	r.Register(File{})
	for name, def := range Names {
		s := def
		if o, ok := lookup(OverrideVar(name)); ok && strings.TrimSpace(o) != "" {
			s = o
		}
		if err := r.Set(name, s); err != nil {
			return nil, fmt.Errorf("%s: %w", OverrideVar(name), err)
		}
	}
	return r, nil
}

// Register adds (or replaces) the provider for its scheme.
func (r *Resolver) Register(p Provider) { r.providers[p.Scheme()] = p }

// Set points name at reference s.
func (r *Resolver) Set(name, s string) error {
	ref, err := ParseRef(s)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refs[name] = ref
	delete(r.cache, name)
	return nil
}

// Ref returns the reference name resolves through (for logs: it holds no value).
func (r *Resolver) Ref(name string) (Ref, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ref, ok := r.refs[name]
	return ref, ok
}

// Get resolves name.
func (r *Resolver) Get(ctx context.Context, name string) (Value, error) {
	r.mu.Lock()
	ref, ok := r.refs[name]
	c, cached := r.cache[name]
	r.mu.Unlock()
	if !ok {
		return Value{}, fmt.Errorf("secret %q: no reference configured (%s): %w", name, OverrideVar(name), ErrNotFound)
	}
	if cached && r.now().Before(c.Expires.Add(-Margin)) {
		return c.Value, nil
	}
	p, ok := r.providers[ref.Scheme]
	if !ok {
		return Value{}, fmt.Errorf("secret %q: no provider for scheme %q (have %s)", name, ref.Scheme, strings.Join(r.schemes(), ", "))
	}
	s, err := p.Get(ctx, ref)
	if err != nil {
		return Value{}, fmt.Errorf("secret %q: %w", name, err)
	}
	if s.Value.Empty() {
		return Value{}, fmt.Errorf("secret %q: %s resolved to an empty value: %w", name, ref, ErrNotFound)
	}
	r.mu.Lock()
	if s.Expires.IsZero() {
		delete(r.cache, name)
	} else {
		r.cache[name] = s
	}
	r.mu.Unlock()
	return s.Value, nil
}

// Has reports whether name currently resolves (errors other than not-found count as present,
// so the caller fails on them later instead of silently choosing another secret).
func (r *Resolver) Has(ctx context.Context, name string) bool {
	_, err := r.Get(ctx, name)
	return !IsNotFound(err)
}

// Source returns a function that resolves name on each call.
func (r *Resolver) Source(name string) Source {
	return func(ctx context.Context) (Value, error) { return r.Get(ctx, name) }
}

func (r *Resolver) schemes() []string {
	out := make([]string, 0, len(r.providers))
	for s := range r.providers {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Source yields a secret on demand. Clients hold a Source, not a value, so a
// rotated secret is picked up on the next request.
type Source func(ctx context.Context) (Value, error)

// Static is a Source for a fixed value (tests, flags).
func Static(s string) Source {
	v := NewValue(s)
	return func(context.Context) (Value, error) {
		if v.Empty() {
			return Value{}, ErrNotFound
		}
		return v, nil
	}
}
