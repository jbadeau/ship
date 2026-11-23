package forges

import (
	"testing"

	"juliusbaer.com/codeak/ship/internal/secret"
)

func lookup(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestFromEnv(t *testing.T) {
	// GitLab CI: forge, URL and context come from predefined variables.
	e, err := FromEnv(lookup(map[string]string{"GITLAB_CI": "true", "CI_SERVER_URL": "https://gl", "CI_PROJECT_PATH": "g/ledger"}))
	if err != nil || e.Type != "gitlab" || e.URL != "https://gl" || e.CI.Repo != "g/ledger" {
		t.Fatalf("%+v %v", e, err)
	}
	// Explicit URL wins; the legacy variable still works.
	e, _ = FromEnv(lookup(map[string]string{"CI_SERVER_URL": "https://gl", "SHIP_GITLAB_URL": "http://gitlab:8929"}))
	if e.URL != "http://gitlab:8929" {
		t.Fatalf("legacy URL: %+v", e)
	}
	e, _ = FromEnv(lookup(map[string]string{"SHIP_FORGE_URL": "https://a", "SHIP_GITLAB_URL": "https://b"}))
	if e.URL != "https://a" || e.Type != "gitlab" {
		t.Fatalf("precedence: %+v", e)
	}
	if _, err := FromEnv(lookup(map[string]string{"SHIP_FORGE": "bitbucket"})); err == nil {
		t.Fatal("unknown forge accepted")
	}
	if _, err := (Env{Type: "gitlab"}).Open(secret.Static("t")); err == nil {
		t.Fatal("Open without URL succeeded")
	}
	f, err := (Env{Type: "gitlab", URL: "https://gl"}).Open(secret.Static("t"))
	if err != nil || f.Name() != "gitlab" {
		t.Fatalf("%v %v", f, err)
	}
}
