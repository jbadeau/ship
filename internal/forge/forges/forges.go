// Package forges selects a forge adapter by name (like driver/builtin for drivers).
package forges

import (
	"fmt"
	"os"
	"strings"

	"juliusbaer.com/codeak/ship/internal/forge"
	"juliusbaer.com/codeak/ship/internal/gitlab"
	"juliusbaer.com/codeak/ship/internal/secret"
)

// Known adapter names.
var Known = []string{gitlab.Name}

// Env is the runtime forge selection, read from the environment:
//
//	SHIP_FORGE      adapter name (default: detected from CI, else gitlab)
//	SHIP_FORGE_URL  server URL (SHIP_GITLAB_URL is accepted for compatibility,
//	                then the CI server URL)
type Env struct {
	Type string
	URL  string
	CI   forge.CI
}

// FromEnv reads Env using lookup (os.Getenv when nil).
func FromEnv(lookup func(string) string) (Env, error) {
	if lookup == nil {
		lookup = os.Getenv
	}
	e := Env{Type: strings.ToLower(strings.TrimSpace(lookup("SHIP_FORGE")))}
	if ci, ok := gitlab.DetectCI(lookup); ok {
		e.CI = ci
	}
	if e.Type == "" {
		e.Type = e.CI.Forge
	}
	if e.Type == "" {
		e.Type = gitlab.Name
	}
	if e.CI.Forge != "" && e.CI.Forge != e.Type {
		e.CI = forge.CI{} // running in another forge's CI: its context does not apply
	}
	for _, k := range []string{"SHIP_FORGE_URL", "SHIP_GITLAB_URL"} {
		if v := lookup(k); v != "" && e.URL == "" {
			e.URL = v
		}
	}
	if e.URL == "" {
		e.URL = e.CI.ServerURL
	}
	if !known(e.Type) {
		return e, fmt.Errorf("SHIP_FORGE=%q: unknown forge (known: %s)", e.Type, strings.Join(Known, ", "))
	}
	return e, nil
}

func known(t string) bool {
	for _, k := range Known {
		if k == t {
			return true
		}
	}
	return false
}

// Open returns a client for the selected forge, authenticated with token.
func (e Env) Open(token secret.Source) (forge.Forge, error) {
	if e.URL == "" {
		return nil, fmt.Errorf("%s: server URL not set (SHIP_FORGE_URL)", e.Type)
	}
	switch e.Type {
	case gitlab.Name:
		return gitlab.New(e.URL, token), nil
	}
	return nil, fmt.Errorf("unknown forge %q", e.Type)
}

// Webhook returns the webhook verifier/parser for the selected forge.
func (e Env) Webhook(s secret.Source, branchPrefix string) (forge.Webhook, error) {
	switch e.Type {
	case gitlab.Name:
		return gitlab.Webhook{Secret: s, BranchPrefix: branchPrefix}, nil
	}
	return nil, fmt.Errorf("unknown forge %q", e.Type)
}
