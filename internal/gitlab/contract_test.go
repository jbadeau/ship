package gitlab

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"juliusbaer.com/codeak/ship/internal/forge"
	"juliusbaer.com/codeak/ship/internal/forge/forgetest"
	"juliusbaer.com/codeak/ship/internal/secret"
)

// TestForgeContract runs the shared forge suite against a real GitLab. It is
// skipped unless SHIP_FORGETEST_GITLAB_URL and SHIP_FORGETEST_GITLAB_TOKEN are
// set (`make forgetest` sets them from the e2e sandbox). The scratch project
// SHIP_FORGETEST_GITLAB_REPO (default forgetest/scratch) is created if missing.
func TestForgeContract(t *testing.T) {
	u, tok := os.Getenv("SHIP_FORGETEST_GITLAB_URL"), os.Getenv("SHIP_FORGETEST_GITLAB_TOKEN")
	if u == "" || tok == "" {
		t.Skip("SHIP_FORGETEST_GITLAB_URL / SHIP_FORGETEST_GITLAB_TOKEN not set")
	}
	repo := os.Getenv("SHIP_FORGETEST_GITLAB_REPO")
	if repo == "" {
		repo = "forgetest/scratch"
	}
	c := New(u, secret.Static(tok))
	ensureProject(t, c, repo)
	group, _, _ := strings.Cut(repo, "/")
	forgetest.Run(t, forgetest.Target{Forge: c, Repo: repo, Group: group})
}

func ensureProject(t *testing.T, c *Client, path string) {
	t.Helper()
	ctx := context.Background()
	if _, err := c.Repo(ctx, path); err == nil {
		return
	} else if !forge.IsNotFound(err) {
		t.Fatal(err)
	}
	group, name, ok := strings.Cut(path, "/")
	if !ok || strings.Contains(name, "/") {
		t.Fatalf("scratch repo %q must be group/name", path)
	}
	var g struct{ ID int }
	if err := c.Do(ctx, "GET", "/groups/"+url.PathEscape(group), nil, &g); IsStatus(err, 404) {
		err = c.Do(ctx, "POST", "/groups", map[string]any{"name": group, "path": group, "visibility": "private"}, &g)
		if err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	if err := c.Do(ctx, "POST", "/projects", map[string]any{
		"name": name, "path": name, "namespace_id": g.ID, "initialize_with_readme": true, "visibility": "private",
	}, nil); err != nil {
		t.Fatal(err)
	}
}
