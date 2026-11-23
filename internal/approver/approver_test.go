package approver

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"juliusbaer.com/codeak/ship/internal/envmr"
	"juliusbaer.com/codeak/ship/internal/forge"
	"juliusbaer.com/codeak/ship/internal/gitlab"
	"juliusbaer.com/codeak/ship/internal/secret"
)

const payload = `{"object_kind":"merge_request","project":{"id":7},
 "object_attributes":{"iid":3,"source_branch":"ship/gw/DEV/abc","state":"opened","action":"open"}}`

func newServer() *Server {
	return &Server{Hook: gitlab.Webhook{Secret: secret.Static("s3cret"), BranchPrefix: BranchPrefix}, Groups: []string{"sda/services"}, Log: log.New(io.Discard, "", 0)}
}

func post(t *testing.T, s *Server, token, body string) int {
	t.Helper()
	req := httptest.NewRequest("POST", "/webhook", strings.NewReader(body))
	req.Header.Set("X-Gitlab-Token", token)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Code
}

func TestWebhook(t *testing.T) {
	s := newServer()
	if c := post(t, s, "wrong", payload); c != http.StatusUnauthorized {
		t.Fatalf("bad secret: got %d", c)
	}
	if c := post(t, s, "", payload); c != http.StatusUnauthorized {
		t.Fatalf("missing secret: got %d", c)
	}
	if s.Pending(Key{"7", 3}) {
		t.Fatal("unauthenticated hook must not enqueue")
	}
	human := strings.Replace(payload, "ship/gw/DEV/abc", "feature/x", 1)
	if c := post(t, s, "s3cret", human); c != http.StatusNoContent || s.Pending(Key{"7", 3}) {
		t.Fatalf("non-ship branch: got %d, pending=%v", c, s.Pending(Key{"7", 3}))
	}
	if c := post(t, s, "s3cret", "{not json"); c != http.StatusBadRequest {
		t.Fatalf("bad json: got %d", c)
	}
	for range 3 { // a burst of events coalesces into one queued check
		if c := post(t, s, "s3cret", payload); c != http.StatusAccepted {
			t.Fatalf("ship MR: got %d", c)
		}
	}
	if !s.Pending(Key{"7", 3}) || len(s.queue) != 1 {
		t.Fatalf("expected exactly one queued item, got %d", len(s.queue))
	}
}

func TestRelevant(t *testing.T) {
	for _, tc := range []struct {
		kind, branch, state, action string
		want                        bool
	}{
		{"merge_request", "ship/a/DEV/x", "opened", "open", true},
		{"merge_request", "ship/a/DEV/x", "opened", "update", true},
		{"merge_request", "ship/a/DEV/x", "opened", "reopen", true},
		{"merge_request", "ship/a/DEV/x", "opened", "approved", false},
		{"merge_request", "ship/a/DEV/x", "merged", "merge", false},
		{"merge_request", "main", "opened", "open", false},
		{"push", "ship/a/DEV/x", "opened", "open", false},
	} {
		if got := gitlab.Relevant(tc.kind, tc.branch, tc.state, tc.action, BranchPrefix); got != tc.want {
			t.Errorf("%+v: got %v", tc, got)
		}
	}
}

func TestInGroups(t *testing.T) {
	g := []string{"sda/services", "sda/applications/"}
	for path, want := range map[string]bool{
		"sda/services/GW/DEV": true, "sda/applications/P/DEV": true,
		"sda/services-evil/GW/DEV": false, "platform/ledger": false, "sda/services": false,
	} {
		if InGroups(path, g) != want {
			t.Errorf("%s: want %v", path, want)
		}
	}
}

func TestProcess(t *testing.T) {
	var notes atomic.Int32
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/notes") {
			notes.Add(1)
		}
		io.WriteString(w, "{}")
	}))
	defer gl.Close()

	s := newServer()
	s.Forge = gitlab.New(gl.URL, secret.Static("t"))
	var verdict envmr.Verdict
	var calls []string
	s.verify = func(_ context.Context, project string, iid int) (envmr.Verdict, error) {
		calls = append(calls, project)
		return verdict, nil
	}
	s.init()
	s.paths["7"], s.paths["8"] = "sda/services/GW/DEV", "other/repo"
	ctx := context.Background()

	// Outside the env groups: never verified.
	s.Process(ctx, Key{"8", 1})
	if len(calls) != 0 {
		t.Fatal("project outside env groups was verified")
	}

	verdict = envmr.Verdict{ShipMR: true, State: forge.StateOpen, SHA: "aaaa", Approved: true, Checks: []string{"ok"}}
	s.Process(ctx, Key{"7", 3})
	if s.Stats.Approved.Load() != 1 || notes.Load() != 0 {
		t.Fatalf("approve: stats=%d notes=%d", s.Stats.Approved.Load(), notes.Load())
	}
	verdict.Already = true // re-check of an approved head: not a new approval
	s.Process(ctx, Key{"7", 3})
	if s.Stats.Approved.Load() != 1 {
		t.Fatalf("re-check counted as approval: %d", s.Stats.Approved.Load())
	}

	// Failures: one note per head SHA, not one per event.
	verdict = envmr.Verdict{ShipMR: true, State: forge.StateOpen, SHA: "bbbb", Failures: []string{"claim differs"}}
	s.Process(ctx, Key{"7", 3})
	s.Process(ctx, Key{"7", 3})
	if notes.Load() != 1 || s.Stats.Rejected.Load() != 2 {
		t.Fatalf("expected 1 note for 2 failed checks of one SHA, got %d", notes.Load())
	}
	verdict.SHA = "cccc"
	s.Process(ctx, Key{"7", 3})
	if notes.Load() != 2 {
		t.Fatalf("new SHA should get a new note, got %d", notes.Load())
	}

	// Merged MRs are skipped silently.
	verdict = envmr.Verdict{ShipMR: true, State: forge.StateMerged, SHA: "dddd", Failures: []string{"x"}}
	s.Process(ctx, Key{"7", 3})
	if notes.Load() != 2 {
		t.Fatal("merged MR must not get a note")
	}
}
