package octopus

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jbadeau/ship/internal/driver"
	"github.com/jbadeau/ship/internal/freight"
)

// fakeOctopus is a tiny in-memory stand-in for the Octopus REST API.
type fakeOctopus struct {
	mu          sync.Mutex
	releases    map[string]release // by version
	deployments []deployment
	tasks       map[string]*task
	finish      string // state new tasks end in: Success or Failed
}

func (f *fakeOctopus) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("X-Octopus-ApiKey") != "key" {
		w.WriteHeader(401)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/api/Spaces-1")
	js := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case r.Method == "GET" && p == "/projects/gw-api":
		js(project{Id: "Projects-1", Name: "gw-api", Slug: "gw-api"})
	case r.Method == "GET" && p == "/environments":
		js(map[string]any{"Items": []environment{{Id: "Environments-3", Name: r.URL.Query().Get("name")}}})
	case r.Method == "GET" && strings.HasPrefix(p, "/projects/Projects-1/releases/"):
		rel, ok := f.releases[strings.TrimPrefix(p, "/projects/Projects-1/releases/")]
		if !ok {
			w.WriteHeader(404)
			return
		}
		js(rel)
	case r.Method == "POST" && p == "/releases":
		var rel release
		_ = json.NewDecoder(r.Body).Decode(&rel)
		rel.Id = "Releases-" + rel.Version
		f.releases[rel.Version] = rel
		js(rel)
	case r.Method == "GET" && p == "/deployments":
		items := []deployment{}
		if n := len(f.deployments); n > 0 {
			items = append(items, f.deployments[n-1])
		}
		js(map[string]any{"Items": items})
	case r.Method == "POST" && p == "/deployments":
		var d deployment
		_ = json.NewDecoder(r.Body).Decode(&d)
		d.Id = "Deployments-" + string(rune('0'+len(f.deployments)))
		d.TaskId = "ServerTasks-" + d.Id
		f.deployments = append(f.deployments, d)
		f.tasks[d.TaskId] = &task{Id: d.TaskId, State: "Executing"}
		js(d)
	case r.Method == "GET" && strings.HasPrefix(p, "/tasks/"):
		t := f.tasks[strings.TrimPrefix(p, "/tasks/")]
		js(t)
		// Complete on the next poll.
		t.IsCompleted, t.State, t.FinishedSuccessfully = true, f.finish, f.finish == "Success"
	default:
		http.Error(w, "unexpected "+r.Method+" "+p, 500)
	}
}

type fakeReg struct {
	freight.Registry
	tags map[string]string
}

func (r fakeReg) TagDigest(_ context.Context, repo, tag string) (string, error) {
	return r.tags[repo+":"+tag], nil
}

func setup(t *testing.T, finish string) (*fakeOctopus, Driver, driver.Request) {
	f := &fakeOctopus{releases: map[string]release{}, tasks: map[string]*task{}, finish: finish}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	dg := "sha256:" + strings.Repeat("1", 64)
	d := Driver{URL: srv.URL, APIKey: "key", PollInterval: time.Millisecond,
		Deps: driver.Deps{Registry: fakeReg{tags: map[string]string{"reg/gw:1.4.2": dg}}}}
	req := driver.Request{App: "SV-API_GW_MGT", Stage: "PROD",
		Freight: freight.Descriptor{Ref: "reg/gw@" + dg, Repository: "reg/gw", Digest: dg,
			Annotations: map[string]string{VersionAnnotation: "1.4.2"}},
		Config: map[string]any{"project": "gw-api", "environment": "Production", "packageStep": "deploy"}}
	return f, d, req
}

func TestApplyWaitIdempotent(t *testing.T) {
	f, d, req := setup(t, "Success")
	ctx := context.Background()
	res, err := d.Apply(ctx, req)
	if err != nil || !res.Changed {
		t.Fatalf("apply: %v %+v", err, res)
	}
	if rel := f.releases["1.4.2"]; rel.SelectedPackages[0].Version != "1.4.2" || !strings.Contains(rel.ReleaseNotes, req.Freight.Digest) {
		t.Fatalf("release: %+v", rel)
	}
	// Retry while deploying: no second deployment.
	if res, err := d.Apply(ctx, req); err != nil || res.Changed {
		t.Fatalf("retry: %v %+v", err, res)
	}
	h, err := d.Wait(ctx, req)
	if err != nil || !h.Ready {
		t.Fatalf("wait: %v %+v", err, h)
	}
	if res, _ := d.Apply(ctx, req); res.Changed || len(f.deployments) != 1 {
		t.Fatalf("expected no-op after success, deployments=%d", len(f.deployments))
	}
	if p, _ := d.Plan(ctx, req); !p.NoOp {
		t.Fatalf("plan should be no-op: %+v", p)
	}
}

func TestWaitFailure(t *testing.T) {
	_, d, req := setup(t, "Failed")
	if _, err := d.Apply(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Wait(context.Background(), req); err == nil {
		t.Fatal("expected failure")
	}
}

func TestMovedTagRejected(t *testing.T) {
	_, d, req := setup(t, "Success")
	d.Deps.Registry = fakeReg{tags: map[string]string{"reg/gw:1.4.2": "sha256:" + strings.Repeat("2", 64)}}
	if _, err := d.Apply(context.Background(), req); err == nil || !strings.Contains(err.Error(), "points to") {
		t.Fatalf("expected tag/digest mismatch, got %v", err)
	}
}

func TestMissingVersion(t *testing.T) {
	_, d, req := setup(t, "Success")
	req.Freight.Annotations = nil
	if _, err := d.Apply(context.Background(), req); err == nil {
		t.Fatal("expected error")
	}
}
