package octopus

import (
	"context"
	"juliusbaer.com/codeak/ship/internal/secret"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"juliusbaer.com/codeak/ship/internal/driver"
	fakeoctopus "juliusbaer.com/codeak/ship/internal/fakes/octopus"
	"juliusbaer.com/codeak/ship/internal/freight"
)

type fakeReg struct {
	freight.Registry
	tags map[string]string
}

func (r fakeReg) TagDigest(_ context.Context, repo, tag string) (string, error) {
	return r.tags[repo+":"+tag], nil
}

// setup runs the driver against the Octopus fake, which validates every request
// and response against the official Octopus Server Swagger document.
func setup(t *testing.T, failEnvs ...string) (*fakeoctopus.Fake, Driver, driver.Request) {
	f := fakeoctopus.New("key")
	f.FailEnvironments = failEnvs
	f.AddProject("Spaces-1", fakeoctopus.Project{Id: "Projects-1", Name: "gw-api", Slug: "gw-api"})
	f.AddEnvironment("Spaces-1", fakeoctopus.Environment{Id: "Environments-3", Name: "Production"})
	f.AddEnvironment("Spaces-1", fakeoctopus.Environment{Id: "Environments-4", Name: "Production-DR"})
	g, err := f.Server()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { g.AssertClean(t) })
	dg := "sha256:" + strings.Repeat("1", 64)
	d := Driver{URL: srv.URL, APIKey: secret.Static("key"), PollInterval: time.Millisecond,
		Deps: driver.Deps{Registry: fakeReg{tags: map[string]string{"reg/gw:1.4.2": dg}}}}
	req := driver.Request{App: "SV-API_GW_MGT", Stage: "PROD",
		Freight: freight.Descriptor{Ref: "reg/gw@" + dg, Repository: "reg/gw", Digest: dg,
			Annotations: map[string]string{VersionAnnotation: "1.4.2"}},
		Config: map[string]any{"project": "gw-api", "environment": "Production", "packageStep": "deploy"}}
	return f, d, req
}

func TestApplyWaitIdempotent(t *testing.T) {
	f, d, req := setup(t)
	ctx := context.Background()
	if p, err := d.Plan(ctx, req); err != nil || p.NoOp || !strings.Contains(p.Summary, "create release 1.4.2") {
		t.Fatalf("plan: %v %+v", err, p)
	}
	res, err := d.Apply(ctx, req)
	if err != nil || !res.Changed {
		t.Fatalf("apply: %v %+v", err, res)
	}
	rels := f.Releases()
	if len(rels) != 1 || rels[0].SelectedPackages[0].Version != "1.4.2" || rels[0].SelectedPackages[0].ActionName != "deploy" ||
		!strings.Contains(rels[0].ReleaseNotes, req.Freight.Digest) {
		t.Fatalf("release: %+v", rels)
	}
	// Retry while deploying: no second deployment.
	if res, err := d.Apply(ctx, req); err != nil || res.Changed {
		t.Fatalf("retry: %v %+v", err, res)
	}
	h, err := d.Wait(ctx, req)
	if err != nil || !h.Ready {
		t.Fatalf("wait: %v %+v", err, h)
	}
	if res, _ := d.Apply(ctx, req); res.Changed || len(f.Deployments()) != 1 {
		t.Fatalf("expected no-op after success, deployments=%d", len(f.Deployments()))
	}
	if p, _ := d.Plan(ctx, req); !p.NoOp {
		t.Fatalf("plan should be no-op: %+v", p)
	}
	// The same release goes to a second environment as a new deployment.
	req.Config["environment"] = "Production-DR"
	if res, err := d.Apply(ctx, req); err != nil || !res.Changed {
		t.Fatalf("second environment: %v %+v", err, res)
	}
	if n := len(f.Releases()); n != 1 {
		t.Fatalf("release must be reused, got %d releases", n)
	}
}

func TestWaitFailure(t *testing.T) {
	_, d, req := setup(t, "Production")
	if _, err := d.Apply(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Wait(context.Background(), req); err == nil || !strings.Contains(err.Error(), "Failed") {
		t.Fatalf("expected failed deployment, got %v", err)
	}
}

func TestUnknownEnvironment(t *testing.T) {
	_, d, req := setup(t)
	req.Config["environment"] = "Nope"
	if _, err := d.Apply(context.Background(), req); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected unknown environment, got %v", err)
	}
}

func TestMovedTagRejected(t *testing.T) {
	_, d, req := setup(t)
	d.Deps.Registry = fakeReg{tags: map[string]string{"reg/gw:1.4.2": "sha256:" + strings.Repeat("2", 64)}}
	if _, err := d.Apply(context.Background(), req); err == nil || !strings.Contains(err.Error(), "points to") {
		t.Fatalf("expected tag/digest mismatch, got %v", err)
	}
}

func TestMissingVersion(t *testing.T) {
	_, d, req := setup(t)
	req.Freight.Annotations = nil
	if _, err := d.Plan(context.Background(), req); err == nil {
		t.Fatal("expected error")
	}
	if _, err := d.Apply(context.Background(), req); err == nil {
		t.Fatal("expected error")
	}
}
