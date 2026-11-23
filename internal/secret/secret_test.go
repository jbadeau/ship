package secret

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestRedaction(t *testing.T) {
	v := NewValue("hunter2-token")
	type wrapper struct {
		Token Value
		P     *Value
	}
	w := wrapper{Token: v, P: &v}
	var sb strings.Builder
	log.New(&sb, "", 0).Printf("%v %+v %#v %s %q %x", v, w, w, v, v, v)
	slog.New(slog.NewTextHandler(&sb, nil)).Info("x", "token", v)
	js, _ := json.Marshal(w)
	sb.Write(js)
	sb.WriteString(fmt.Sprint(v) + fmt.Sprintf("%d", v))
	out := sb.String()
	if strings.Contains(out, "hunter2") {
		t.Fatalf("secret leaked: %s", out)
	}
	if !strings.Contains(out, "[redacted]") || v.Reveal() != "hunter2-token" {
		t.Fatal("redaction marker missing or value lost")
	}
}

func TestParseRef(t *testing.T) {
	for in, want := range map[string]Ref{
		"env://OCTOPUS_API_KEY":         {"env", "OCTOPUS_API_KEY", ""},
		"file:///var/run/secrets/token": {"file", "/var/run/secrets/token", ""},
		"vault://kv/data/ship/octo#key": {"vault", "kv/data/ship/octo", "key"},
		"  env://X  ":                   {"env", "X", ""},
	} {
		got, err := ParseRef(in)
		if err != nil || got != want {
			t.Errorf("%q: %+v %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "OCTOPUS_API_KEY", "env://", "://x"} {
		if _, err := ParseRef(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestDefaultsAndOverrides(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "octo")
	if err := os.WriteFile(f, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := NewResolver(env(map[string]string{
		"SHIP_GITLAB_TOKEN":           "from-env",
		"SHIP_SECRET_OCTOPUS_API_KEY": "file://" + f,
		"OCTOPUS_API_KEY":             "ignored: overridden",
		"SHIP_SECRET_WEBHOOK_SECRET":  "env://OTHER_VAR",
	}))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if v, err := r.Get(ctx, ForgeToken); err != nil || v.Reveal() != "from-env" {
		t.Fatalf("default env ref: %v %v", v.Reveal(), err)
	}
	if v, err := r.Get(ctx, OctopusAPIKey); err != nil || v.Reveal() != "from-file" {
		t.Fatalf("file override: %q %v", v.Reveal(), err)
	}
	// Fail closed: the override points at an unset variable; no fallback to the default.
	if _, err := r.Get(ctx, WebhookSecret); !IsNotFound(err) {
		t.Fatalf("want not found, got %v", err)
	}
	if r.Has(ctx, ForgeReadToken) {
		t.Fatal("unset read token reported present")
	}
	if _, err := r.Get(ctx, "no-such-secret"); !IsNotFound(err) {
		t.Fatalf("unknown name: %v", err)
	}
	// Errors name the reference, never a value.
	_, err = r.Get(ctx, WebhookSecret)
	if !strings.Contains(err.Error(), "env://OTHER_VAR") {
		t.Fatalf("error should name the reference: %v", err)
	}
	if _, err := NewResolver(env(map[string]string{"SHIP_SECRET_FORGE_TOKEN": "not-a-ref"})); err == nil ||
		!strings.Contains(err.Error(), "SHIP_SECRET_FORGE_TOKEN") {
		t.Fatalf("bad override: %v", err)
	}
	if err := r.Set(ForgeToken, "vault://kv/x"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(ctx, ForgeToken); err == nil || !strings.Contains(err.Error(), `no provider for scheme "vault"`) {
		t.Fatalf("unknown scheme: %v", err)
	}
}

func TestFileRotation(t *testing.T) {
	f := filepath.Join(t.TempDir(), "tok")
	_ = os.WriteFile(f, []byte("v1"), 0o600)
	r, _ := NewResolver(env(nil))
	_ = r.Set("t", "file://"+f)
	src := r.Source("t")
	if v, _ := src(context.Background()); v.Reveal() != "v1" {
		t.Fatal(v.Reveal())
	}
	_ = os.WriteFile(f, []byte("v2\n"), 0o600)
	if v, _ := src(context.Background()); v.Reveal() != "v2" {
		t.Fatalf("rotated file not picked up: %s", v.Reveal())
	}
	_ = os.WriteFile(f, []byte("\n"), 0o600)
	if _, err := src(context.Background()); !IsNotFound(err) {
		t.Fatalf("empty file must not resolve: %v", err)
	}
	_ = os.Remove(f)
	if _, err := src(context.Background()); !IsNotFound(err) {
		t.Fatalf("missing file: %v", err)
	}
}

type leased struct {
	calls int
	ttl   time.Duration
}

func (*leased) Scheme() string { return "lease" }
func (l *leased) Get(_ context.Context, ref Ref) (Secret, error) {
	l.calls++
	return Secret{Value: NewValue(fmt.Sprintf("%s-%d", ref.Path, l.calls)), Expires: time.Unix(0, 0).Add(l.ttl)}, nil
}

func TestExpiringSecretsAreCached(t *testing.T) {
	p := &leased{ttl: time.Hour}
	r, _ := NewResolver(env(nil))
	r.Register(p)
	now := time.Unix(0, 0)
	r.now = func() time.Time { return now }
	_ = r.Set("dyn", "lease://tok")
	ctx := context.Background()
	a, _ := r.Get(ctx, "dyn")
	b, _ := r.Get(ctx, "dyn")
	if p.calls != 1 || !a.Equal(b) {
		t.Fatalf("expected one provider call, got %d", p.calls)
	}
	now = now.Add(time.Hour - Margin) // inside the refresh margin
	c, _ := r.Get(ctx, "dyn")
	if p.calls != 2 || c.Equal(a) {
		t.Fatalf("expected refresh before expiry, calls=%d", p.calls)
	}
}
