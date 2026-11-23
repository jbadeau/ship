package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"juliusbaer.com/codeak/ship/internal/forge"
	"juliusbaer.com/codeak/ship/internal/secret"
)

func TestMRConversion(t *testing.T) {
	for _, tc := range []struct {
		state, want string
		merges      int
	}{
		{"opened", forge.StateOpen, 0}, {"locked", forge.StateOpen, 0},
		{"closed", forge.StateClosed, 0}, {"merged", forge.StateMerged, 3},
	} {
		m := MR{IID: 4, ProjectID: 9, State: tc.state, SHA: "head", MergeCommitSHA: "merge", SquashCommitSHA: "squash",
			Author: User{Username: "bot"}, DiffRefs: DiffRefs{BaseSHA: "base"}, HeadPipeline: &Pipeline{ID: 1, Status: "failed", WebURL: "p"}}
		cr := m.CR()
		if cr.State != tc.want || len(cr.MergeSHAs) != tc.merges || cr.ID != 4 || cr.RepoID != "9" ||
			cr.Author != "bot" || cr.BaseSHA != "base" || cr.HeadSHA != "head" || cr.Pipeline.Status != forge.RunFailed {
			t.Errorf("%s: %+v", tc.state, cr)
		}
	}
	// Merged CRs list the merge commit first: it is what the deploy pipeline runs on.
	if got := (MR{State: "merged", SHA: "h", MergeCommitSHA: "m"}).CR().MergeSHAs; got[0] != "m" || got[1] != "h" {
		t.Errorf("merge SHA order: %v", got)
	}
}

func TestPaginationAndQuery(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != "tok" {
			w.WriteHeader(401)
			return
		}
		queries = append(queries, r.URL.RawQuery)
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		n := 100
		if page == 3 {
			n = 7
		} else {
			w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
		}
		out := make([]MR, n)
		for i := range out {
			out[i] = MR{IID: page*1000 + i, State: "opened"}
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()
	c := New(srv.URL, secret.Static("tok"))
	crs, err := c.ListCRs(context.Background(), "g/p", forge.CRQuery{SourceBranch: "ship/x", Label: "ship"})
	if err != nil {
		t.Fatal(err)
	}
	if len(crs) != 207 || len(queries) != 3 {
		t.Fatalf("got %d CRs in %d requests", len(crs), len(queries))
	}
	for _, want := range []string{"state=opened", "source_branch=ship%2Fx", "labels=ship", "sort=desc", "per_page=100"} {
		if !strings.Contains(queries[0], want) {
			t.Errorf("query %q lacks %s", queries[0], want)
		}
	}
}

func TestErrorsAndToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"404 Not found"}`, 404)
	}))
	defer srv.Close()
	ctx := context.Background()
	_, err := New(srv.URL, secret.Static("t")).CR(ctx, "g/p", 1)
	if !forge.IsNotFound(err) {
		t.Fatalf("404 should be forge.ErrNotFound: %v", err)
	}
	_, err = New(srv.URL, secret.Static("")).CR(ctx, "g/p", 1)
	if !secret.IsNotFound(err) || !strings.Contains(err.Error(), "gitlab token") {
		t.Fatalf("missing token: %v", err)
	}
	if err := New(srv.URL, secret.Static("t")).ReportCheck(ctx, "g/p", 1, "sha", forge.Check{Key: "v8"}); err == nil {
		t.Fatal("non-numeric check key must be rejected")
	}
}

func TestTokenIsResolvedPerRequest(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("PRIVATE-TOKEN"))
		_, _ = w.Write([]byte(`{"username":"x"}`))
	}))
	defer srv.Close()
	tok := "first"
	c := New(srv.URL, func(context.Context) (secret.Value, error) { return secret.NewValue(tok), nil })
	_, _ = c.Whoami(context.Background())
	tok = "rotated"
	_, _ = c.Whoami(context.Background())
	if strings.Join(seen, ",") != "first,rotated" {
		t.Fatalf("tokens sent: %v", seen)
	}
}

func TestWebhook(t *testing.T) {
	w := Webhook{Secret: secret.Static("s3cret-s3cret-s3"), BranchPrefix: "ship/"}
	req := httptest.NewRequest("POST", "/webhook", nil)
	if err := w.Verify(req, nil); err == nil {
		t.Fatal("missing token accepted")
	}
	req.Header.Set("X-Gitlab-Token", "s3cret-s3cret-s3")
	if err := w.Verify(req, nil); err != nil {
		t.Fatal(err)
	}
	body := `{"object_kind":"merge_request","project":{"id":7,"path_with_namespace":"evil/claimed"},
	 "object_attributes":{"iid":3,"source_branch":"ship/gw/DEV/abc","state":"opened","action":"update"}}`
	repo, cr, ok, err := w.Parse([]byte(body))
	if err != nil || !ok || repo != "7" || cr != 3 {
		t.Fatalf("Parse = %q %d %v %v (the repo must be the id, never the claimed path)", repo, cr, ok, err)
	}
	if _, _, ok, _ := w.Parse([]byte(strings.Replace(body, "ship/", "feature/", 1))); ok {
		t.Fatal("non-ship branch is relevant")
	}
	if _, _, _, err := w.Parse([]byte("{")); err == nil {
		t.Fatal("bad JSON accepted")
	}
	broken := Webhook{Secret: func(context.Context) (secret.Value, error) { return secret.Value{}, errors.New("vault down") }}
	if err := broken.Verify(req, nil); err == nil {
		t.Fatal("unresolvable secret must fail closed")
	}
}

func TestDetectCI(t *testing.T) {
	env := map[string]string{"GITLAB_CI": "true", "CI_SERVER_URL": "https://gl", "CI_PROJECT_PATH": "g/p",
		"CI_MERGE_REQUEST_IID": "12", "CI_MERGE_REQUEST_DIFF_BASE_SHA": "b", "CI_COMMIT_BEFORE_SHA": "o", "CI_PIPELINE_URL": "u"}
	ci, ok := DetectCI(func(k string) string { return env[k] })
	if !ok || ci != (forge.CI{Forge: Name, ServerURL: "https://gl", Repo: "g/p", CR: 12, BaseSHA: "b", BeforeSHA: "o", RunURL: "u"}) {
		t.Fatalf("%+v %v", ci, ok)
	}
	if _, ok := DetectCI(func(string) string { return "" }); ok {
		t.Fatal("detected GitLab CI in an empty environment")
	}
}
