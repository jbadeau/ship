package main

import "testing"

func TestRepoFromRemote(t *testing.T) {
	for in, want := range map[string]string{
		"https://gitlab.example/platform/ledger.git":   "platform/ledger",
		"http://oauth2:x@gitlab:8929/platform/ledger":  "platform/ledger",
		"ssh://git@gitlab.example:2222/a/b/ledger.git": "a/b/ledger",
		"git@github.com:org/ledger.git\n":              "org/ledger",
		"/srv/git/ledger.git":                          "",
		"https://gitlab.example":                       "",
	} {
		if got := repoFromRemote(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}
