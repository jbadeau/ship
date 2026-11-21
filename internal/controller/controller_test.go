package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/jbadeau/ship/api/v1alpha1"
	"github.com/jbadeau/ship/internal/freight"
	"github.com/jbadeau/ship/internal/gitlab"
	"github.com/jbadeau/ship/internal/testutil"
)

// ---------- fake GitLab (only what the controller uses) ----------

type fakeMR struct {
	gitlab.MR
	approvers []string
	commits   []gitlab.Commit
	notes     []string
}

type fakeGitLab struct {
	mu        sync.Mutex
	mrs       map[int]*fakeMR
	files     map[string]string // branch:path -> content
	pipelines map[string][]gitlab.Pipeline
	checks    []string // "iid:sha:status"
}

func newFakeGitLab() *fakeGitLab {
	return &fakeGitLab{mrs: map[int]*fakeMR{}, files: map[string]string{}, pipelines: map[string][]gitlab.Pipeline{}}
}

func (f *fakeGitLab) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := strings.TrimPrefix(r.URL.EscapedPath(), "/api/v4/projects/platform%2Fledger")
	js := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	mrIID := func() (*fakeMR, string) {
		rest := strings.TrimPrefix(p, "/merge_requests/")
		id, sub, _ := strings.Cut(rest, "/")
		n, _ := strconv.Atoi(id)
		return f.mrs[n], sub
	}
	switch {
	case r.Method == "GET" && p == "/merge_requests":
		q := r.URL.Query()
		out := []gitlab.MR{}
		for i := len(f.mrs); i >= 1; i-- {
			m := f.mrs[i]
			if (q.Get("source_branch") == "" || m.SourceBranch == q.Get("source_branch")) &&
				(q.Get("state") == "" || q.Get("state") == "all" || q.Get("state") == m.State) {
				out = append(out, m.MR)
			}
		}
		js(out)
	case r.Method == "GET" && strings.HasPrefix(p, "/repository/branches/"):
		w.WriteHeader(404)
	case r.Method == "POST" && p == "/repository/commits":
		acts := body["actions"].([]any)
		a := acts[0].(map[string]any)
		f.files[body["branch"].(string)+":"+a["file_path"].(string)] = a["content"].(string)
		f.files[body["branch"].(string)+":message"] = body["commit_message"].(string)
		js(map[string]any{"id": "c1"})
	case r.Method == "POST" && p == "/merge_requests":
		n := len(f.mrs) + 1
		src := body["source_branch"].(string)
		f.mrs[n] = &fakeMR{MR: gitlab.MR{IID: n, State: "opened", SourceBranch: src, TargetBranch: "main",
			Title: body["title"].(string), Description: body["description"].(string), SHA: fmt.Sprintf("sha%d", n),
			Author: gitlab.User{Username: "ship-promoter"}, WebURL: fmt.Sprintf("http://gl/mr/%d", n)},
			commits: []gitlab.Commit{{ID: "c1", Message: f.files[src+":message"]}}}
		js(f.mrs[n].MR)
	case strings.HasPrefix(p, "/merge_requests/"):
		m, sub := mrIID()
		if m == nil {
			w.WriteHeader(404)
			return
		}
		switch {
		case r.Method == "GET" && sub == "":
			js(m.MR)
		case r.Method == "GET" && sub == "commits":
			js(m.commits)
		case r.Method == "GET" && sub == "approvals":
			var by []map[string]any
			for _, a := range m.approvers {
				by = append(by, map[string]any{"user": map[string]any{"username": a}})
			}
			js(map[string]any{"approved_by": by})
		case r.Method == "POST" && sub == "notes":
			m.notes = append(m.notes, body["body"].(string))
			js(map[string]any{})
		case r.Method == "PUT" && sub == "":
			if body["state_event"] == "close" {
				m.State = "closed"
			}
			js(m.MR)
		case r.Method == "POST" && sub == "status_check_responses":
			f.checks = append(f.checks, fmt.Sprintf("%d:%s:%s", m.IID, body["sha"], body["status"]))
			js(map[string]any{})
		default:
			http.Error(w, "unexpected "+r.Method+" "+p, 500)
		}
	case r.Method == "GET" && p == "/pipelines":
		ps := f.pipelines[r.URL.Query().Get("sha")]
		if ps == nil {
			ps = []gitlab.Pipeline{}
		}
		js(ps)
	default:
		http.Error(w, "unexpected "+r.Method+" "+p, 500)
	}
}

func (f *fakeGitLab) mr(n int) *fakeMR { f.mu.Lock(); defer f.mu.Unlock(); return f.mrs[n] }
func (f *fakeGitLab) set(fn func())    { f.mu.Lock(); defer f.mu.Unlock(); fn() }

// ---------- fixture ----------

const artifactType = "application/vnd.example.deploy-bundle.v1"

type fixture struct {
	t    *testing.T
	gl   *fakeGitLab
	host string
	r    *Reconciler
	c    client.Client
	git  string // ledger working repo (origin)
}

func setup(t *testing.T) *fixture {
	host := testutil.Registry(t)
	repo := testutil.GitRepo(t)
	shipYAML := fmt.Sprintf(`apiVersion: ship/v1alpha1
gitlab: { botUsername: ship-bot, promoterUsername: ship-promoter, approverUsername: ship-approver }
identities: { "alice@bank.example": alice }
apps:
  gw: { repository: %s/deploy/gw, deploy: { driver: noop } }
stages:
  - name: DEV
  - name: PROD
    upstream: DEV
`, host)
	testutil.Commit(t, repo, "init", map[string]string{"ship.yaml": shipYAML})

	gl := newFakeGitLab()
	srv := httptest.NewServer(gl)
	t.Cleanup(srv.Close)

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.Promotion{}).Build()
	cache := &LedgerCache{Dir: filepath.Join(t.TempDir(), "ledger.git"), URL: repo.Dir, Branch: "main"}
	return &fixture{t: t, gl: gl, host: host, c: c, git: repo.Dir, r: &Reconciler{
		Client: c, GitLab: gitlab.New(srv.URL, "tok"), Reg: freight.NewOCI([]string{host}),
		LedgerProject: "platform/ledger", Ledger: cache, Poll: time.Second}}
}

var uidN int

func (f *fixture) create(name, stage, ref, by string) {
	f.t.Helper()
	uidN++
	p := &v1alpha1.Promotion{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team", UID: types.UID(fmt.Sprintf("%08d-uid", uidN)),
			CreationTimestamp: metav1.NewTime(time.Now().Add(time.Duration(uidN) * time.Second))},
		Spec: v1alpha1.PromotionSpec{App: "gw", Stage: stage, Freight: ref, RequestedBy: by},
	}
	if err := f.c.Create(context.Background(), p); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) reconcile(name string) *v1alpha1.Promotion {
	f.t.Helper()
	ctx := context.Background()
	for i := 0; i < 3; i++ { // first pass adds the finalizer
		if _, err := f.r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "team", Name: name}}); err != nil {
			f.t.Fatal(err)
		}
	}
	var p v1alpha1.Promotion
	if err := f.c.Get(ctx, types.NamespacedName{Namespace: "team", Name: name}, &p); err != nil {
		return nil
	}
	return &p
}

func cond(p *v1alpha1.Promotion, typ string) metav1.ConditionStatus {
	if c := meta.FindStatusCondition(p.Status.Conditions, typ); c != nil {
		return c.Status
	}
	return ""
}

// ---------- tests ----------

func TestPromotionLifecycle(t *testing.T) {
	f := setup(t)
	a := testutil.PushArtifact(t, f.host+"/deploy/gw", "1.0.0", artifactType, nil)

	f.create("gw-dev", "DEV", a, "alice@bank.example")
	p := f.reconcile("gw-dev")
	if p.Status.Phase != v1alpha1.PhaseProposed || p.Status.LedgerMR == nil {
		t.Fatalf("expected Proposed with MR, got %+v", p.Status)
	}
	m := f.gl.mr(p.Status.LedgerMR.IID)
	for _, want := range []string{"Requested-by: alice@bank.example", "Promotion: team/gw-dev", "Promotion-UID: " + string(p.UID), "Freight: " + a} {
		if !strings.Contains(m.Description, want) || !strings.Contains(m.commits[0].Message, want) {
			t.Errorf("MR/commit missing trailer %q", want)
		}
	}
	if got := f.gl.files[m.SourceBranch+":ledger/gw/DEV.yaml"]; got != "freight: "+a+"\n" {
		t.Errorf("pin content %q", got)
	}

	// Validated; requester approves own promotion (mapped identity) -> V8 not satisfied.
	f.gl.set(func() {
		m.HeadPipeline = &gitlab.Pipeline{Status: "success", WebURL: "http://gl/p/1"}
		m.approvers = []string{"alice"}
	})
	p = f.reconcile("gw-dev")
	if p.Status.Phase != v1alpha1.PhaseAwaitingApproval || cond(p, v1alpha1.CondApproved) != metav1.ConditionFalse {
		t.Fatalf("expected AwaitingApproval + Approved=False, got %s %v", p.Status.Phase, p.Status.Conditions)
	}

	// Status check (pre-merge V8) reports failed, then passed after an independent approval.
	sc := &StatusChecker{Reader: f.c, GitLab: f.r.GitLab, LedgerProject: "platform/ledger", Ledger: f.r.Ledger, CheckID: 7}
	if err := sc.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.gl.set(func() { m.approvers = []string{"alice", "bob"} })
	if err := sc.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := sc.Once(context.Background()); err != nil { // unchanged -> not re-reported
		t.Fatal(err)
	}
	if got := strings.Join(f.gl.checks, ","); got != "1:sha1:failed,1:sha1:passed" {
		t.Fatalf("status checks: %s", got)
	}
	p = f.reconcile("gw-dev")
	if cond(p, v1alpha1.CondApproved) != metav1.ConditionTrue {
		t.Fatalf("expected Approved=True, got %v", p.Status.Conditions)
	}

	// Merged -> Deploying -> Succeeded.
	f.gl.set(func() { m.State, m.MergeCommitSHA = "merged", "merge1" })
	if p = f.reconcile("gw-dev"); p.Status.Phase != v1alpha1.PhaseDeploying || p.Status.LedgerCommit != "merge1" {
		t.Fatalf("expected Deploying, got %+v", p.Status)
	}
	f.gl.set(func() {
		f.gl.pipelines["merge1"] = []gitlab.Pipeline{{ID: 9, Status: "success", WebURL: "http://gl/p/9"}}
	})
	if p = f.reconcile("gw-dev"); p.Status.Phase != v1alpha1.PhaseSucceeded || cond(p, v1alpha1.CondDeployed) != metav1.ConditionTrue {
		t.Fatalf("expected Succeeded, got %+v", p.Status)
	}
	if len(p.Finalizers) != 0 {
		t.Fatalf("finalizer should be removed once terminal: %v", p.Finalizers)
	}
}

func TestRejectAndDeleteAndSupersede(t *testing.T) {
	f := setup(t)
	a := testutil.PushArtifact(t, f.host+"/deploy/gw", "1.0.0", artifactType, nil)
	b := testutil.PushArtifact(t, f.host+"/deploy/gw", "1.1.0", artifactType, nil)

	// Skipping DEV: rejected by the pre-check, no MR opened.
	f.create("skip", "PROD", a, "alice")
	if p := f.reconcile("skip"); p.Status.Phase != v1alpha1.PhaseRejected || p.Status.LedgerMR != nil ||
		!strings.HasPrefix(p.Status.Violations[0], "V5") {
		t.Fatalf("expected Rejected/V5, got %+v", p.Status)
	}

	// Tag-free short digest form resolves against the app repository.
	f.create("old", "DEV", a[strings.Index(a, "@")+1:], "alice")
	old := f.reconcile("old")
	if old.Status.Freight != a {
		t.Fatalf("freight not resolved: %s", old.Status.Freight)
	}
	// A newer Promotion for the same app/stage supersedes the open one.
	f.create("new", "DEV", b, "alice")
	nw := f.reconcile("new")
	if old = f.reconcile("old"); old.Status.Phase != v1alpha1.PhaseSuperseded || f.gl.mr(old.Status.LedgerMR.IID).State != "closed" {
		t.Fatalf("expected old Superseded with closed MR, got %s", old.Status.Phase)
	}

	// Deleting a Promotion with an open MR closes the MR.
	if err := f.c.Delete(context.Background(), nw); err != nil {
		t.Fatal(err)
	}
	if p := f.reconcile("new"); p != nil {
		t.Fatalf("expected Promotion gone, got finalizers %v", p.Finalizers)
	}
	if m := f.gl.mr(nw.Status.LedgerMR.IID); m.State != "closed" || !strings.Contains(m.notes[0], "was deleted") {
		t.Fatalf("expected MR closed on delete, got %s %v", m.State, m.notes)
	}
}

func TestStatusCheckRequiresLivePromotion(t *testing.T) {
	f := setup(t)
	a := testutil.PushArtifact(t, f.host+"/deploy/gw", "1.0.0", artifactType, nil)
	f.create("gw", "DEV", a, "alice")
	p := f.reconcile("gw")
	m := f.gl.mr(p.Status.LedgerMR.IID)
	f.gl.set(func() {
		m.approvers = []string{"bob"}
		m.Description = strings.Replace(m.Description, string(p.UID), "forged-uid", 1)
	})
	sc := &StatusChecker{Reader: f.c, GitLab: f.r.GitLab, LedgerProject: "platform/ledger", Ledger: f.r.Ledger, CheckID: 7}
	ok, why, err := sc.Evaluate(context.Background(), m.MR)
	if err != nil || ok || !strings.Contains(why, "UID mismatch") {
		t.Fatalf("expected UID mismatch failure, got ok=%v why=%q err=%v", ok, why, err)
	}
}
