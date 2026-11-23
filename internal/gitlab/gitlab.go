// Package gitlab is ship's GitLab adapter: a minimal REST client that
// implements forge.Forge, plus GitLab CI context and webhook handling.
package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"juliusbaer.com/codeak/ship/internal/forge"
	"juliusbaer.com/codeak/ship/internal/secret"
)

// Name is the adapter name used in ship.yaml (forge.type) and CI detection.
const Name = "gitlab"

type Client struct {
	BaseURL string        // e.g. https://gitlab.example.com
	Token   secret.Source // resolved per request, so rotation needs no restart
	HTTP    *http.Client
}

var _ forge.Forge = (*Client)(nil)

func New(baseURL string, token secret.Source) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Client) Name() string { return Name }

type APIError struct {
	Status int
	Method string
	Path   string
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("gitlab %s %s: %d: %s", e.Method, e.Path, e.Status, strings.TrimSpace(e.Body))
}

// Unwrap lets callers use forge.IsNotFound.
func (e *APIError) Unwrap() error {
	if e.Status == http.StatusNotFound {
		return forge.ErrNotFound
	}
	return nil
}

func IsStatus(err error, status int) bool {
	e, ok := err.(*APIError)
	return ok && e.Status == status
}

// P encodes a project path or id for use in a URL.
func P(project string) string { return url.QueryEscape(project) }

func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	_, err := c.do(ctx, method, path, body, out)
	return err
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) (http.Header, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+"/api/v4"+path, rd)
	if err != nil {
		return nil, err
	}
	if c.Token == nil {
		return nil, fmt.Errorf("gitlab: no token configured")
	}
	tok, err := c.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("gitlab token: %w", err)
	}
	req.Header.Set("PRIVATE-TOKEN", tok.Reveal())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return resp.Header, &APIError{Status: resp.StatusCode, Method: method, Path: path, Body: string(b)}
	}
	if out == nil {
		return resp.Header, nil
	}
	if raw, ok := out.(*[]byte); ok {
		*raw = b
		return resp.Header, nil
	}
	return resp.Header, json.Unmarshal(b, out)
}

// all follows GitLab pagination (X-Next-Page) and returns every item.
func all[T any](ctx context.Context, c *Client, path string, q url.Values) ([]T, error) {
	if q == nil {
		q = url.Values{}
	}
	q.Set("per_page", "100")
	var out []T
	for page := 1; page > 0; {
		q.Set("page", strconv.Itoa(page))
		var batch []T
		h, err := c.do(ctx, "GET", path+"?"+q.Encode(), nil, &batch)
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
		next, _ := strconv.Atoi(h.Get("X-Next-Page"))
		if next <= page || len(batch) == 0 {
			break
		}
		page = next
	}
	return out, nil
}

// ---- wire types (GitLab JSON) ----

type User struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
}

type Project struct {
	ID                int    `json:"id"`
	PathWithNamespace string `json:"path_with_namespace"`
	DefaultBranch     string `json:"default_branch"`
	WebURL            string `json:"web_url"`
	HTTPURLToRepo     string `json:"http_url_to_repo"`
}

type Pipeline struct {
	ID     int    `json:"id"`
	Status string `json:"status"`
	WebURL string `json:"web_url"`
}

type DiffRefs struct {
	BaseSHA  string `json:"base_sha"`
	HeadSHA  string `json:"head_sha"`
	StartSHA string `json:"start_sha"`
}

type MR struct {
	ID                        int       `json:"id"`
	IID                       int       `json:"iid"`
	ProjectID                 int       `json:"project_id"`
	Title                     string    `json:"title"`
	Description               string    `json:"description"`
	State                     string    `json:"state"` // opened, closed, merged, locked
	SourceBranch              string    `json:"source_branch"`
	TargetBranch              string    `json:"target_branch"`
	SHA                       string    `json:"sha"`
	MergeCommitSHA            string    `json:"merge_commit_sha"`
	SquashCommitSHA           string    `json:"squash_commit_sha"`
	Author                    User      `json:"author"`
	Labels                    []string  `json:"labels"`
	WebURL                    string    `json:"web_url"`
	DetailedMergeStatus       string    `json:"detailed_merge_status"`
	HasConflicts              bool      `json:"has_conflicts"`
	HeadPipeline              *Pipeline `json:"head_pipeline"`
	DiffRefs                  DiffRefs  `json:"diff_refs"`
	MergeWhenPipelineSucceeds bool      `json:"merge_when_pipeline_succeeds"`
}

type Diff struct {
	OldPath     string `json:"old_path"`
	NewPath     string `json:"new_path"`
	NewFile     bool   `json:"new_file"`
	RenamedFile bool   `json:"renamed_file"`
	DeletedFile bool   `json:"deleted_file"`
}

type Commit struct {
	ID          string `json:"id"`
	Message     string `json:"message"`
	AuthorEmail string `json:"author_email"`
}

// ---- conversions ----

func state(s string) string {
	switch s {
	case "opened", "locked":
		return forge.StateOpen
	case "closed":
		return forge.StateClosed
	case "merged":
		return forge.StateMerged
	}
	return s
}

func (m MR) CR() forge.ChangeRequest {
	cr := forge.ChangeRequest{
		ID: m.IID, Title: m.Title, Description: m.Description, State: state(m.State),
		SourceBranch: m.SourceBranch, TargetBranch: m.TargetBranch, HeadSHA: m.SHA, BaseSHA: m.DiffRefs.BaseSHA,
		Author: m.Author.Username, Labels: m.Labels, URL: m.WebURL,
		AutoMerge: m.MergeWhenPipelineSucceeds, HasConflicts: m.HasConflicts,
	}
	if m.ProjectID != 0 {
		cr.RepoID = strconv.Itoa(m.ProjectID)
	}
	if cr.State == forge.StateMerged {
		for _, s := range []string{m.MergeCommitSHA, m.SquashCommitSHA, m.SHA} {
			if s != "" {
				cr.MergeSHAs = append(cr.MergeSHAs, s)
			}
		}
	}
	if m.HeadPipeline != nil {
		r := m.HeadPipeline.run()
		cr.Pipeline = &r
	}
	return cr
}

func (p Pipeline) run() forge.Run { return forge.Run{ID: p.ID, Status: p.Status, URL: p.WebURL} }

func crs(mrs []MR) []forge.ChangeRequest {
	out := make([]forge.ChangeRequest, len(mrs))
	for i, m := range mrs {
		out[i] = m.CR()
	}
	return out
}

func query(q forge.CRQuery) url.Values {
	v := url.Values{"order_by": {"created_at"}, "sort": {"desc"}}
	switch q.State {
	case "", forge.StateOpen:
		v.Set("state", "opened")
	case forge.StateAll:
		v.Set("state", "all")
	default:
		v.Set("state", q.State)
	}
	if q.SourceBranch != "" {
		v.Set("source_branch", q.SourceBranch)
	}
	if q.TargetBranch != "" {
		v.Set("target_branch", q.TargetBranch)
	}
	if q.Label != "" {
		v.Set("labels", q.Label)
	}
	return v
}

// ---- forge.Forge ----

func (c *Client) Whoami(ctx context.Context) (string, error) {
	var u User
	err := c.Do(ctx, "GET", "/user", nil, &u)
	return u.Username, err
}

func (c *Client) GitCredentials(ctx context.Context) (string, string, error) {
	tok, err := c.Token(ctx)
	if err != nil {
		return "", "", err
	}
	return "oauth2", tok.Reveal(), nil
}

func (c *Client) Repo(ctx context.Context, project string) (forge.Repo, error) {
	var p Project
	if err := c.Do(ctx, "GET", "/projects/"+P(project), nil, &p); err != nil {
		return forge.Repo{}, err
	}
	return forge.Repo{ID: strconv.Itoa(p.ID), Path: p.PathWithNamespace, DefaultBranch: p.DefaultBranch, WebURL: p.WebURL, CloneURL: p.HTTPURLToRepo}, nil
}

// File returns raw file content at ref; found=false on 404.
func (c *Client) File(ctx context.Context, project, path, ref string) ([]byte, bool, error) {
	var b []byte
	err := c.Do(ctx, "GET", "/projects/"+P(project)+"/repository/files/"+url.PathEscape(path)+"/raw?ref="+url.QueryEscape(ref), nil, &b)
	if IsStatus(err, 404) {
		return nil, false, nil
	}
	return b, err == nil, err
}

func (c *Client) BranchSHA(ctx context.Context, project, branch string) (string, bool, error) {
	var b struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	err := c.Do(ctx, "GET", "/projects/"+P(project)+"/repository/branches/"+url.PathEscape(branch), nil, &b)
	if IsStatus(err, 404) {
		return "", false, nil
	}
	return b.Commit.ID, err == nil, err
}

func (c *Client) DeleteBranch(ctx context.Context, project, branch string) error {
	return c.Do(ctx, "DELETE", "/projects/"+P(project)+"/repository/branches/"+url.PathEscape(branch), nil, nil)
}

// IsAncestor reports whether commit is reachable from ref.
func (c *Client) IsAncestor(ctx context.Context, project, commit, ref string) (bool, error) {
	var mb struct {
		ID string `json:"id"`
	}
	err := c.Do(ctx, "GET", "/projects/"+P(project)+"/repository/merge_base?refs[]="+url.QueryEscape(commit)+"&refs[]="+url.QueryEscape(ref), nil, &mb)
	if err != nil {
		return false, err
	}
	return mb.ID == commit, nil
}

func (c *Client) Commit(ctx context.Context, project string, s forge.CommitSpec) error {
	type action struct {
		Action   string `json:"action"`
		FilePath string `json:"file_path"`
		Content  string `json:"content"`
	}
	acts := make([]action, 0, len(s.Files))
	for _, f := range s.Files {
		a := "update"
		if f.Create {
			a = "create"
		}
		acts = append(acts, action{a, f.Path, f.Content})
	}
	body := map[string]any{"branch": s.Branch, "commit_message": s.Message, "actions": acts}
	if s.StartBranch != "" { // only when creating the branch; GitLab rejects an empty value
		body["start_branch"] = s.StartBranch
	}
	return c.Do(ctx, "POST", "/projects/"+P(project)+"/repository/commits", body, nil)
}

func (c *Client) CR(ctx context.Context, project string, iid int) (forge.ChangeRequest, error) {
	var mr MR
	err := c.Do(ctx, "GET", fmt.Sprintf("/projects/%s/merge_requests/%d", P(project), iid), nil, &mr)
	return mr.CR(), err
}

func (c *Client) ListCRs(ctx context.Context, project string, q forge.CRQuery) ([]forge.ChangeRequest, error) {
	mrs, err := all[MR](ctx, c, "/projects/"+P(project)+"/merge_requests", query(q))
	return crs(mrs), err
}

// GroupCRs lists merge requests of a group and its subgroups.
func (c *Client) GroupCRs(ctx context.Context, group string, q forge.CRQuery) ([]forge.ChangeRequest, error) {
	mrs, err := all[MR](ctx, c, "/groups/"+P(group)+"/merge_requests", query(q))
	return crs(mrs), err
}

func (c *Client) OpenCR(ctx context.Context, project string, s forge.CRSpec) (forge.ChangeRequest, error) {
	var mr MR
	err := c.Do(ctx, "POST", "/projects/"+P(project)+"/merge_requests", map[string]any{
		"source_branch": s.SourceBranch, "target_branch": s.TargetBranch, "title": s.Title,
		"description": s.Description, "labels": strings.Join(s.Labels, ","), "remove_source_branch": s.RemoveSourceBranch,
	}, &mr)
	return mr.CR(), err
}

func (c *Client) CloseCR(ctx context.Context, project string, iid int, comment string) error {
	if comment != "" {
		if err := c.Comment(ctx, project, iid, comment); err != nil {
			return err
		}
	}
	return c.Do(ctx, "PUT", fmt.Sprintf("/projects/%s/merge_requests/%d", P(project), iid), map[string]any{"state_event": "close"}, nil)
}

func (c *Client) Comment(ctx context.Context, project string, iid int, body string) error {
	return c.Do(ctx, "POST", fmt.Sprintf("/projects/%s/merge_requests/%d/notes", P(project), iid), map[string]any{"body": body}, nil)
}

// AutoMerge sets "merge when pipeline succeeds" (or merges now if it already succeeded).
func (c *Client) AutoMerge(ctx context.Context, project string, iid int, sha string) error {
	return c.Do(ctx, "PUT", fmt.Sprintf("/projects/%s/merge_requests/%d/merge", P(project), iid), map[string]any{
		"sha": sha, "auto_merge": true, "merge_when_pipeline_succeeds": true, "should_remove_source_branch": true,
	}, nil)
}

func (c *Client) Diffs(ctx context.Context, project string, iid int) ([]forge.FileDiff, error) {
	ds, err := all[Diff](ctx, c, fmt.Sprintf("/projects/%s/merge_requests/%d/diffs", P(project), iid), nil)
	out := make([]forge.FileDiff, len(ds))
	for i, d := range ds {
		out[i] = forge.FileDiff{OldPath: d.OldPath, NewPath: d.NewPath, Created: d.NewFile, Renamed: d.RenamedFile, Deleted: d.DeletedFile}
	}
	return out, err
}

func (c *Client) Approvers(ctx context.Context, project string, iid int) ([]string, error) {
	var a struct {
		ApprovedBy []struct {
			User User `json:"user"`
		} `json:"approved_by"`
	}
	if err := c.Do(ctx, "GET", fmt.Sprintf("/projects/%s/merge_requests/%d/approvals", P(project), iid), nil, &a); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(a.ApprovedBy))
	for _, x := range a.ApprovedBy {
		out = append(out, x.User.Username)
	}
	return out, nil
}

// Approve approves exactly the given head sha (GitLab rejects it if the MR moved).
func (c *Client) Approve(ctx context.Context, project string, iid int, sha string) error {
	return c.Do(ctx, "POST", fmt.Sprintf("/projects/%s/merge_requests/%d/approve", P(project), iid), map[string]any{"sha": sha}, nil)
}

// CRsForCommit lists merge requests that introduced a commit.
func (c *Client) CRsForCommit(ctx context.Context, project, sha string) ([]forge.ChangeRequest, error) {
	var mrs []MR
	err := c.Do(ctx, "GET", "/projects/"+P(project)+"/repository/commits/"+url.PathEscape(sha)+"/merge_requests", nil, &mrs)
	return crs(mrs), err
}

// CRCommits lists the commits of a merge request.
func (c *Client) CRCommits(ctx context.Context, project string, iid int) ([]forge.Commit, error) {
	cs, err := all[Commit](ctx, c, fmt.Sprintf("/projects/%s/merge_requests/%d/commits", P(project), iid), nil)
	out := make([]forge.Commit, len(cs))
	for i, x := range cs {
		out[i] = forge.Commit{SHA: x.ID, Message: x.Message}
	}
	return out, err
}

// Runs lists pipelines for a commit, newest first.
func (c *Client) Runs(ctx context.Context, project, sha string) ([]forge.Run, error) {
	var ps []Pipeline
	if err := c.Do(ctx, "GET", "/projects/"+P(project)+"/pipelines?order_by=id&sort=desc&sha="+url.QueryEscape(sha), nil, &ps); err != nil {
		return nil, err
	}
	out := make([]forge.Run, len(ps))
	for i, p := range ps {
		out[i] = p.run()
	}
	return out, nil
}

// ReportCheck answers a GitLab external status check (Ultimate); Key is its id.
func (c *Client) ReportCheck(ctx context.Context, project string, iid int, sha string, ch forge.Check) error {
	id, err := strconv.Atoi(ch.Key)
	if err != nil || id <= 0 {
		return fmt.Errorf("gitlab: check key %q is not an external status check id", ch.Key)
	}
	status := "failed"
	if ch.Passed {
		status = "passed"
	}
	return c.Do(ctx, "POST", fmt.Sprintf("/projects/%s/merge_requests/%d/status_check_responses", P(project), iid),
		map[string]any{"sha": sha, "external_status_check_id": id, "status": status}, nil)
}
