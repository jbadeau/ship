// Package octopus implements the octopus driver (docs/design.md §7.5): one Octopus
// release per Freight, deployed to the stage's environment. The ship ledger owns
// "what runs where"; the Octopus project owns "how to deploy".
package octopus

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"juliusbaer.com/codeak/ship/internal/secret"
	"net/http"
	"net/url"
	"strings"
	"time"

	"juliusbaer.com/codeak/ship/internal/config"
	"juliusbaer.com/codeak/ship/internal/driver"
)

// VersionAnnotation is the descriptor annotation used for the release and package version.
const VersionAnnotation = "org.opencontainers.image.version"

type Driver struct {
	Deps         driver.Deps
	URL          string
	APIKey       secret.Source // the octopus-api-key secret
	HTTP         *http.Client
	PollInterval time.Duration
}

type target struct {
	Space, Project, Environment, PackageStep, PackageRef string
}

func targetFromConfig(cfg map[string]any) (target, error) {
	var t target
	var err error
	if t.Project, err = config.String(cfg, "project"); err != nil {
		return t, err
	}
	if t.Environment, err = config.String(cfg, "environment"); err != nil {
		return t, err
	}
	if t.PackageStep, err = config.String(cfg, "packageStep"); err != nil {
		return t, err
	}
	t.Space = config.StringOr(cfg, "space", "Spaces-1")
	t.PackageRef = config.StringOr(cfg, "packageReference", "")
	return t, nil
}

func (d Driver) do(ctx context.Context, method, path string, body, out any) error {
	if d.URL == "" || d.APIKey == nil {
		return fmt.Errorf("octopus driver: OCTOPUS_URL and the octopus-api-key secret (OCTOPUS_API_KEY) are required")
	}
	key, err := d.APIKey(ctx)
	if err != nil {
		return fmt.Errorf("octopus driver: %w", err)
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(d.URL, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("X-Octopus-ApiKey", key.Reveal())
	req.Header.Set("Content-Type", "application/json")
	hc := d.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 404 {
		return errNotFound
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("octopus %s %s: %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

var errNotFound = fmt.Errorf("not found")

type project struct{ Id, Name, Slug string }
type environment struct{ Id, Name string }
type selectedPackage struct {
	ActionName           string
	PackageReferenceName string `json:",omitempty"`
	Version              string
}
type release struct {
	Id, Version, ProjectId, ReleaseNotes string
	SelectedPackages                     []selectedPackage
}

// createRelease is Octopus's CreateReleaseCommand (only the fields ship sets).
type createRelease struct {
	SpaceId          string
	ProjectId        string
	Version          string
	ReleaseNotes     string
	SelectedPackages []selectedPackage
}

// createDeployment is Octopus's CreateDeploymentCommand (only the fields ship sets).
type createDeployment struct {
	SpaceId       string
	ReleaseId     string
	EnvironmentId string
	Comments      string
}

type deployment struct {
	Id, ReleaseId, EnvironmentId, TaskId, Comments string
}
type task struct {
	Id, State, ErrorMessage           string
	IsCompleted, FinishedSuccessfully bool
}

type resolved struct {
	t       target
	version string
	project project
	env     environment
}

func (d Driver) resolve(ctx context.Context, r driver.Request) (resolved, error) {
	t, err := targetFromConfig(r.Config)
	if err != nil {
		return resolved{}, err
	}
	version := r.Freight.Annotations[VersionAnnotation]
	if version == "" {
		return resolved{}, fmt.Errorf("octopus driver: freight %s has no %s annotation (needed for release and package version)", r.Freight.Short(), VersionAnnotation)
	}
	// Close the tag/digest gap: Octopus selects packages by version (tag), so the
	// tag must still point at exactly the pinned digest.
	if d.Deps.Registry != nil {
		dg, err := d.Deps.Registry.TagDigest(ctx, r.Freight.Repository, version)
		if err != nil {
			return resolved{}, fmt.Errorf("octopus driver: package tag %s: %w", version, err)
		}
		if dg != r.Freight.Digest {
			return resolved{}, fmt.Errorf("octopus driver: tag %s:%s points to %s, not the pinned %s (mutable tag?)", r.Freight.Repository, version, dg, r.Freight.Digest)
		}
	}
	var p project
	if err := d.do(ctx, "GET", fmt.Sprintf("/api/%s/projects/%s", t.Space, url.PathEscape(t.Project)), nil, &p); err != nil {
		return resolved{}, fmt.Errorf("octopus project %q: %w", t.Project, err)
	}
	var envs struct{ Items []environment }
	if err := d.do(ctx, "GET", fmt.Sprintf("/api/%s/environments?name=%s", t.Space, url.QueryEscape(t.Environment)), nil, &envs); err != nil {
		return resolved{}, err
	}
	var env *environment
	for i := range envs.Items {
		if envs.Items[i].Name == t.Environment {
			env = &envs.Items[i]
		}
	}
	if env == nil {
		return resolved{}, fmt.Errorf("octopus environment %q not found", t.Environment)
	}
	return resolved{t: t, version: version, project: p, env: *env}, nil
}

func (d Driver) findRelease(ctx context.Context, rs resolved) (*release, error) {
	var rel release
	err := d.do(ctx, "GET", fmt.Sprintf("/api/%s/projects/%s/releases/%s", rs.t.Space, rs.project.Id, url.PathEscape(rs.version)), nil, &rel)
	if err == errNotFound {
		return nil, nil
	}
	return &rel, err
}

// latestDeployment returns the newest deployment of the project to the environment.
func (d Driver) latestDeployment(ctx context.Context, rs resolved) (*deployment, error) {
	var res struct{ Items []deployment }
	q := url.Values{"projects": {rs.project.Id}, "environments": {rs.env.Id}, "take": {"1"}}
	if err := d.do(ctx, "GET", fmt.Sprintf("/api/%s/deployments?%s", rs.t.Space, q.Encode()), nil, &res); err != nil {
		return nil, err
	}
	if len(res.Items) == 0 {
		return nil, nil
	}
	return &res.Items[0], nil
}

func (d Driver) task(ctx context.Context, space, id string) (task, error) {
	var t task
	return t, d.do(ctx, "GET", fmt.Sprintf("/api/%s/tasks/%s", space, id), nil, &t)
}

func (d Driver) Plan(ctx context.Context, r driver.Request) (driver.Plan, error) {
	rs, err := d.resolve(ctx, r)
	if err != nil {
		return driver.Plan{}, err
	}
	rel, err := d.findRelease(ctx, rs)
	if err != nil {
		return driver.Plan{}, err
	}
	action := "create release " + rs.version
	if rel != nil {
		action = "reuse release " + rs.version
	}
	cur := "none"
	if dep, _ := d.latestDeployment(ctx, rs); dep != nil {
		cur = dep.ReleaseId
		if rel != nil && dep.ReleaseId == rel.Id {
			if t, err := d.task(ctx, rs.t.Space, dep.TaskId); err == nil && t.FinishedSuccessfully {
				return driver.Plan{NoOp: true, Summary: fmt.Sprintf("Octopus %s/%s already runs release %s", rs.project.Name, rs.env.Name, rs.version)}, nil
			}
		}
	}
	return driver.Plan{Summary: fmt.Sprintf("Octopus project %s: %s (package step %q = %s), deploy to %s (currently %s)",
		rs.project.Name, action, rs.t.PackageStep, rs.version, rs.env.Name, cur)}, nil
}

func (d Driver) Apply(ctx context.Context, r driver.Request) (driver.Result, error) {
	rs, err := d.resolve(ctx, r)
	if err != nil {
		return driver.Result{}, err
	}
	rel, err := d.findRelease(ctx, rs)
	if err != nil {
		return driver.Result{}, err
	}
	notes := fmt.Sprintf("Freight: %s\nDigest: %s", r.Freight.Ref, r.Freight.Digest)
	if rel == nil {
		in := createRelease{SpaceId: rs.t.Space, ProjectId: rs.project.Id, Version: rs.version, ReleaseNotes: notes,
			SelectedPackages: []selectedPackage{{ActionName: rs.t.PackageStep, PackageReferenceName: rs.t.PackageRef, Version: rs.version}}}
		var out release
		if err := d.do(ctx, "POST", fmt.Sprintf("/api/%s/releases", rs.t.Space), in, &out); err != nil {
			return driver.Result{}, fmt.Errorf("create release: %w", err)
		}
		rel = &out
		d.Deps.Logf("created Octopus release %s (%s)", rel.Version, rel.Id)
	} else if !strings.Contains(rel.ReleaseNotes, r.Freight.Digest) {
		// Same version, different digest: the version is not a unique identity.
		return driver.Result{}, fmt.Errorf("Octopus release %s exists but was created for a different digest", rs.version)
	}
	if dep, err := d.latestDeployment(ctx, rs); err != nil {
		return driver.Result{}, err
	} else if dep != nil && dep.ReleaseId == rel.Id {
		t, err := d.task(ctx, rs.t.Space, dep.TaskId)
		if err != nil {
			return driver.Result{}, err
		}
		if t.FinishedSuccessfully || !t.IsCompleted {
			return driver.Result{Summary: fmt.Sprintf("release %s already deployed/deploying to %s (%s)", rs.version, rs.env.Name, dep.Id)}, nil
		}
	}
	comments := fmt.Sprintf("ship promotion of %s to %s. Ledger MR: %s Pipeline: %s", r.App, r.Stage, r.Promotion.LedgerMRURL, r.Promotion.PipelineURL)
	var dep deployment
	if err := d.do(ctx, "POST", fmt.Sprintf("/api/%s/deployments", rs.t.Space),
		createDeployment{SpaceId: rs.t.Space, ReleaseId: rel.Id, EnvironmentId: rs.env.Id, Comments: comments}, &dep); err != nil {
		return driver.Result{}, fmt.Errorf("create deployment: %w", err)
	}
	d.Deps.Logf("started Octopus deployment %s (task %s)", dep.Id, dep.TaskId)
	return driver.Result{Changed: true, Summary: fmt.Sprintf("deploying release %s to %s (%s)", rs.version, rs.env.Name, dep.Id)}, nil
}

func (d Driver) Wait(ctx context.Context, r driver.Request) (driver.Health, error) {
	rs, err := d.resolve(ctx, r)
	if err != nil {
		return driver.Health{}, err
	}
	rel, err := d.findRelease(ctx, rs)
	if err != nil || rel == nil {
		return driver.Health{}, fmt.Errorf("release %s not found: %v", rs.version, err)
	}
	poll := d.PollInterval
	if poll == 0 {
		poll = 5 * time.Second
	}
	for {
		dep, err := d.latestDeployment(ctx, rs)
		if err != nil {
			return driver.Health{}, err
		}
		if dep == nil || dep.ReleaseId != rel.Id {
			return driver.Health{}, fmt.Errorf("latest deployment to %s is not release %s", rs.env.Name, rs.version)
		}
		t, err := d.task(ctx, rs.t.Space, dep.TaskId)
		if err != nil {
			return driver.Health{}, err
		}
		if t.IsCompleted {
			if t.FinishedSuccessfully {
				return driver.Health{Ready: true, Message: fmt.Sprintf("release %s deployed to %s", rs.version, rs.env.Name)}, nil
			}
			return driver.Health{}, fmt.Errorf("deployment %s %s: %s", dep.Id, t.State, t.ErrorMessage)
		}
		d.Deps.Logf("waiting for Octopus task %s (%s)", t.Id, t.State)
		select {
		case <-ctx.Done():
			return driver.Health{}, ctx.Err()
		case <-time.After(poll):
		}
	}
}
