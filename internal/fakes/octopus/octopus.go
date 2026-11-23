// Package octopus is a stateful fake of the Octopus Deploy REST API, covering the
// operations ship's octopus driver uses, behind a contract.Guard built from the
// official Octopus Server Swagger document (internal/fakes/specs).
//
// Behaviour: projects and environments are seeded; releases and deployments are
// created through the API; a deployment's task goes Queued → Executing → Success
// (or Failed for environments listed in FailEnvironments) as it is polled.
package octopus

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/getkin/kin-openapi/openapi3"

	"juliusbaer.com/codeak/ship/internal/fakes/contract"
	"juliusbaer.com/codeak/ship/internal/fakes/specs"
)

type Project struct {
	Id      string `json:"Id" yaml:"id"`
	Name    string `json:"Name" yaml:"name"`
	Slug    string `json:"Slug" yaml:"slug"`
	SpaceId string `json:"SpaceId" yaml:"-"`
}

type Environment struct {
	Id      string `json:"Id" yaml:"id"`
	Name    string `json:"Name" yaml:"name"`
	SpaceId string `json:"SpaceId" yaml:"-"`
}

type SelectedPackage struct {
	ActionName           string `json:"ActionName,omitempty"`
	PackageReferenceName string `json:"PackageReferenceName,omitempty"`
	StepName             string `json:"StepName,omitempty"`
	Version              string `json:"Version"`
}

type Release struct {
	Id               string            `json:"Id"`
	SpaceId          string            `json:"SpaceId"`
	ProjectId        string            `json:"ProjectId"`
	Version          string            `json:"Version"`
	ReleaseNotes     string            `json:"ReleaseNotes,omitempty"`
	SelectedPackages []SelectedPackage `json:"SelectedPackages"`
}

type Deployment struct {
	Id            string `json:"Id"`
	SpaceId       string `json:"SpaceId"`
	ProjectId     string `json:"ProjectId"`
	ReleaseId     string `json:"ReleaseId"`
	EnvironmentId string `json:"EnvironmentId"`
	TaskId        string `json:"TaskId"`
	Comments      string `json:"Comments,omitempty"`
	Name          string `json:"Name"`
	Created       string `json:"Created"`
}

type Task struct {
	Id                   string `json:"Id"`
	SpaceId              string `json:"SpaceId"`
	Name                 string `json:"Name"`
	Description          string `json:"Description"`
	State                string `json:"State"`
	IsCompleted          bool   `json:"IsCompleted"`
	FinishedSuccessfully bool   `json:"FinishedSuccessfully"`
	ErrorMessage         string `json:"ErrorMessage,omitempty"`

	polls int
	fail  bool
}

// Fake holds the state of one Octopus server.
type Fake struct {
	APIKey string
	// TaskPolls is how many task GETs a deployment needs to complete (default 2).
	TaskPolls int
	// FailEnvironments: deployments to these environment names fail.
	FailEnvironments []string

	mu          sync.Mutex
	projects    map[string]map[string]*Project // space → id → project
	envs        map[string]map[string]*Environment
	releases    []*Release
	deployments []*Deployment
	tasks       map[string]*Task
	seq         int
}

func New(apiKey string) *Fake {
	return &Fake{APIKey: apiKey, TaskPolls: 2, projects: map[string]map[string]*Project{},
		envs: map[string]map[string]*Environment{}, tasks: map[string]*Task{}}
}

func (f *Fake) AddProject(space string, p Project) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.projects[space] == nil {
		f.projects[space] = map[string]*Project{}
	}
	p.SpaceId = space
	f.projects[space][p.Id] = &p
}

func (f *Fake) AddEnvironment(space string, e Environment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.envs[space] == nil {
		f.envs[space] = map[string]*Environment{}
	}
	e.SpaceId = space
	f.envs[space][e.Id] = &e
}

// Releases and Deployments return snapshots for assertions.
func (f *Fake) Releases() []Release {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Release, len(f.releases))
	for i, r := range f.releases {
		out[i] = *r
	}
	return out
}

func (f *Fake) Deployments() []Deployment {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Deployment, len(f.deployments))
	for i, d := range f.deployments {
		out[i] = *d
	}
	return out
}

func (f *Fake) next(prefix string) string {
	f.seq++
	return prefix + "-" + strconv.Itoa(f.seq)
}

var (
	docOnce sync.Once
	doc     *openapi3.T
	docErr  error
)

// Server returns the fake behind a contract guard for the official spec.
func (f *Fake) Server() (*contract.Guard, error) {
	docOnce.Do(func() { doc, docErr = contract.LoadSwagger2(specs.Octopus, "/api") })
	if docErr != nil {
		return nil, docErr
	}
	return contract.New("octopus", doc, f.handler())
}

func (f *Fake) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/{space}/projects/{idOrSlug}", f.getProject)
	mux.HandleFunc("GET /api/{space}/environments", f.listEnvironments)
	mux.HandleFunc("GET /api/{space}/projects/{projectId}/releases/{version}", f.getRelease)
	mux.HandleFunc("POST /api/{space}/releases", f.createRelease)
	mux.HandleFunc("GET /api/{space}/deployments", f.listDeployments)
	mux.HandleFunc("POST /api/{space}/deployments", f.createDeployment)
	mux.HandleFunc("GET /api/{space}/tasks/{id}", f.getTask)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-Octopus-ApiKey")
		if key == "" {
			key = r.URL.Query().Get("apikey")
		}
		if f.APIKey != "" && key != f.APIKey {
			writeErr(w, http.StatusUnauthorized, "You must be logged in to perform this action.")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr returns Octopus's Error resource.
func writeErr(w http.ResponseWriter, code int, msg string, errs ...string) {
	if errs == nil {
		errs = []string{}
	}
	writeJSON(w, code, map[string]any{"ErrorMessage": msg, "Errors": errs})
}

func (f *Fake) getProject(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := r.PathValue("idOrSlug")
	for _, p := range f.projects[r.PathValue("space")] {
		if p.Id == id || p.Slug == id {
			writeJSON(w, 200, p)
			return
		}
	}
	writeErr(w, 404, "The resource you requested was not found.")
}

func collection[T any](itemType string, items []T) map[string]any {
	if items == nil {
		items = []T{}
	}
	return map[string]any{"ItemType": itemType, "Items": items, "TotalResults": len(items),
		"ItemsPerPage": 30, "NumberOfPages": 1, "LastPageNumber": 0}
}

func take[T any](r *http.Request, items []T) []T {
	if n, err := strconv.Atoi(r.URL.Query().Get("take")); err == nil && n >= 0 && n < len(items) {
		return items[:n]
	}
	return items
}

func (f *Fake) listEnvironments(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := r.URL.Query().Get("name")
	var out []Environment
	for _, e := range f.envs[r.PathValue("space")] {
		if name == "" || strings.EqualFold(e.Name, name) {
			out = append(out, *e)
		}
	}
	slices.SortFunc(out, func(a, b Environment) int { return strings.Compare(a.Id, b.Id) })
	writeJSON(w, 200, collection("Environment", take(r, out)))
}

func (f *Fake) getRelease(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rel := range f.releases {
		if rel.SpaceId == r.PathValue("space") && rel.ProjectId == r.PathValue("projectId") && rel.Version == r.PathValue("version") {
			writeJSON(w, 200, rel)
			return
		}
	}
	writeErr(w, 404, "The resource you requested was not found.")
}

func (f *Fake) createRelease(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SpaceId, ProjectId, Version, ReleaseNotes, ChannelId string
		SelectedPackages                                     []SelectedPackage
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, "invalid body", err.Error())
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	space := r.PathValue("space")
	if in.SpaceId != space {
		writeErr(w, 400, "There was a problem with your request.", fmt.Sprintf("SpaceId %q does not match the URL space %q", in.SpaceId, space))
		return
	}
	if f.projects[space][in.ProjectId] == nil {
		writeErr(w, 400, "There was a problem with your request.", "Project "+in.ProjectId+" not found")
		return
	}
	for _, rel := range f.releases {
		if rel.ProjectId == in.ProjectId && rel.Version == in.Version {
			writeErr(w, 400, "There was a problem with your request.", "A release with version "+in.Version+" already exists for this project.")
			return
		}
	}
	rel := &Release{Id: f.next("Releases"), SpaceId: space, ProjectId: in.ProjectId, Version: in.Version,
		ReleaseNotes: in.ReleaseNotes, SelectedPackages: in.SelectedPackages}
	if rel.SelectedPackages == nil {
		rel.SelectedPackages = []SelectedPackage{}
	}
	f.releases = append(f.releases, rel)
	writeJSON(w, 201, rel)
}

func (f *Fake) listDeployments(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	match := func(vals []string, v string) bool {
		var all []string
		for _, s := range vals {
			all = append(all, strings.Split(s, ",")...)
		}
		return len(all) == 0 || slices.Contains(all, v)
	}
	var out []Deployment
	for i := len(f.deployments) - 1; i >= 0; i-- { // newest first, like Octopus
		d := f.deployments[i]
		if d.SpaceId == r.PathValue("space") && match(q["projects"], d.ProjectId) && match(q["environments"], d.EnvironmentId) {
			out = append(out, *d)
		}
	}
	writeJSON(w, 200, collection("Deployment", take(r, out)))
}

func (f *Fake) createDeployment(w http.ResponseWriter, r *http.Request) {
	var in struct{ ReleaseId, EnvironmentId, Comments string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, "invalid body", err.Error())
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	space := r.PathValue("space")
	var rel *Release
	for _, x := range f.releases {
		if x.Id == in.ReleaseId && x.SpaceId == space {
			rel = x
		}
	}
	env := f.envs[space][in.EnvironmentId]
	if rel == nil || env == nil {
		writeErr(w, 400, "There was a problem with your request.", "unknown release or environment")
		return
	}
	t := &Task{Id: f.next("ServerTasks"), SpaceId: space, Name: "Deploy",
		Description: fmt.Sprintf("Deploy release %s to %s", rel.Version, env.Name), State: "Queued",
		fail: slices.Contains(f.FailEnvironments, env.Name)}
	f.tasks[t.Id] = t
	d := &Deployment{Id: f.next("Deployments"), SpaceId: space, ProjectId: rel.ProjectId, ReleaseId: rel.Id,
		EnvironmentId: env.Id, TaskId: t.Id, Comments: in.Comments,
		Name: "Deploy to " + env.Name, Created: time.Now().UTC().Format(time.RFC3339)}
	f.deployments = append(f.deployments, d)
	writeJSON(w, 201, d)
}

func (f *Fake) getTask(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.tasks[r.PathValue("id")]
	if t == nil || t.SpaceId != r.PathValue("space") {
		writeErr(w, 404, "The resource you requested was not found.")
		return
	}
	if !t.IsCompleted {
		t.polls++
		switch {
		case t.polls >= f.TaskPolls && t.fail:
			t.State, t.IsCompleted, t.ErrorMessage = "Failed", true, "The deployment failed (fake: environment configured to fail)."
		case t.polls >= f.TaskPolls:
			t.State, t.IsCompleted, t.FinishedSuccessfully = "Success", true, true
		default:
			t.State = "Executing"
		}
	}
	writeJSON(w, 200, t)
}
