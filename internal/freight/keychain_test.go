package freight

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"

	"juliusbaer.com/codeak/ship/internal/secret"
)

func authFor(t *testing.T, kc authn.Keychain, ref string) *authn.AuthConfig {
	t.Helper()
	r, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	a, err := authn.Resolve(context.Background(), kc, r.Context())
	if err != nil {
		t.Fatal(err)
	}
	if a == authn.Anonymous {
		return nil
	}
	c, err := a.Authorization()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSecretKeychain(t *testing.T) {
	cfg := `{"auths":{
	  "https://registry.example:5000/v2/": {"auth":"` + base64.StdEncoding.EncodeToString([]byte("bot:pw1")) + `"},
	  "ghcr.io": {"username":"u","password":"pw2"},
	  "https://index.docker.io/v1/": {"identitytoken":"idt"}}}`
	kc := SecretKeychain{Source: secret.Static(cfg)}
	if c := authFor(t, kc, "registry.example:5000/app@sha256:"+strings.Repeat("a", 64)); c == nil || c.Username != "bot" || c.Password != "pw1" {
		t.Fatalf("auth field: %+v", c)
	}
	if c := authFor(t, kc, "ghcr.io/o/app:1"); c == nil || c.Password != "pw2" {
		t.Fatalf("username/password: %+v", c)
	}
	if c := authFor(t, kc, "library/busybox:1"); c == nil || c.IdentityToken != "idt" {
		t.Fatalf("docker hub key: %+v", c)
	}
	if c := authFor(t, kc, "other.example/app:1"); c != nil {
		t.Fatalf("unknown registry should be anonymous: %+v", c)
	}
	// Unset secret: anonymous, so the multi-keychain falls through to Docker's.
	if c := authFor(t, SecretKeychain{Source: secret.Static("")}, "ghcr.io/o/app:1"); c != nil {
		t.Fatalf("unset secret: %+v", c)
	}
}

func TestSecretKeychainFailsClosed(t *testing.T) {
	r, _ := name.ParseReference("ghcr.io/o/app:1")
	for _, src := range []secret.Source{
		func(context.Context) (secret.Value, error) { return secret.Value{}, errors.New("vault down") },
		secret.Static(`not json "hunter2"`),
	} {
		_, err := authn.Resolve(context.Background(), Keychain(src), r.Context())
		if err == nil {
			t.Fatal("error must not fall through to another keychain")
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Fatalf("secret leaked: %v", err)
		}
	}
}
