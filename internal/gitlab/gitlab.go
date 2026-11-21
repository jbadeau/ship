// Package gitlab is a minimal GitLab REST client covering what ship needs.
package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	BaseURL string // e.g. https://gitlab.example.com
	Token   string
	HTTP    *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

type APIError struct {
	Status int
	Method string
	Path   string
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("gitlab %s %s: %d: %s", e.Method, e.Path, e.Status, strings.TrimSpace(e.Body))
}

func IsStatus(err error, status int) bool {
	e, ok := err.(*APIError)
	return ok && e.Status == status
}

// P encodes a project path or id for use in a URL.
func P(project string) string { return url.QueryEscape(project) }

func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+"/api/v4"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("PRIVATE-TOKEN", c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return &APIError{Status: resp.StatusCode, Method: method, Path: path, Body: string(b)}
	}
	if out == nil {
		return nil
	}
	if raw, ok := out.(*[]byte); ok {
		*raw = b
		return nil
	}
	return json.Unmarshal(b, out)
}

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

func (c *Client) CurrentUser(ctx context.Context) (User, error) {
	var u User
	return u, c.Do(ctx, "GET", "/user", nil, &u)
}

func (c *Client) Project(ctx context.Context, project string) (Project, error) {
	var p Project
	return p, c.Do(ctx, "GET", "/projects/"+P(project), nil, &p)
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

type CommitAction struct {
	Action   string `json:"action"` // create, update
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

func (c *Client) CreateCommit(ctx context.Context, project, branch, startBranch, message string, actions []CommitAction) error {
	return c.Do(ctx, "POST", "/projects/"+P(project)+"/repository/commits", map[string]any{
		"branch": branch, "start_branch": startBranch, "commit_message": message, "actions": actions,
	}, nil)
}

func (c *Client) ListMRs(ctx context.Context, project string, q url.Values) ([]MR, error) {
	var mrs []MR
	return mrs, c.Do(ctx, "GET", "/projects/"+P(project)+"/merge_requests?"+q.Encode(), nil, &mrs)
}

func (c *Client) MR(ctx context.Context, project string, iid int) (MR, error) {
	var mr MR
	return mr, c.Do(ctx, "GET", fmt.Sprintf("/projects/%s/merge_requests/%d", P(project), iid), nil, &mr)
}

func (c *Client) CreateMR(ctx context.Context, project string, fields map[string]any) (MR, error) {
	var mr MR
	return mr, c.Do(ctx, "POST", "/projects/"+P(project)+"/merge_requests", fields, &mr)
}

func (c *Client) CloseMR(ctx context.Context, project string, iid int, note string) error {
	if note != "" {
		if err := c.Note(ctx, project, iid, note); err != nil {
			return err
		}
	}
	return c.Do(ctx, "PUT", fmt.Sprintf("/projects/%s/merge_requests/%d", P(project), iid), map[string]any{"state_event": "close"}, nil)
}

func (c *Client) Note(ctx context.Context, project string, iid int, body string) error {
	return c.Do(ctx, "POST", fmt.Sprintf("/projects/%s/merge_requests/%d/notes", P(project), iid), map[string]any{"body": body}, nil)
}

// AutoMerge sets "merge when pipeline succeeds" (or merges now if it already succeeded).
func (c *Client) AutoMerge(ctx context.Context, project string, iid int, sha string) error {
	return c.Do(ctx, "PUT", fmt.Sprintf("/projects/%s/merge_requests/%d/merge", P(project), iid), map[string]any{
		"sha": sha, "auto_merge": true, "merge_when_pipeline_succeeds": true, "should_remove_source_branch": true,
	}, nil)
}

func (c *Client) Diffs(ctx context.Context, project string, iid int) ([]Diff, error) {
	var d []Diff
	return d, c.Do(ctx, "GET", fmt.Sprintf("/projects/%s/merge_requests/%d/diffs?per_page=100", P(project), iid), nil, &d)
}

func (c *Client) ApprovedBy(ctx context.Context, project string, iid int) ([]string, error) {
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

// CommitMRs lists merge requests that introduced a commit.
func (c *Client) CommitMRs(ctx context.Context, project, sha string) ([]MR, error) {
	var mrs []MR
	return mrs, c.Do(ctx, "GET", "/projects/"+P(project)+"/repository/commits/"+sha+"/merge_requests", nil, &mrs)
}

type Commit struct {
	ID          string `json:"id"`
	Message     string `json:"message"`
	AuthorEmail string `json:"author_email"`
}

// MRCommits lists the commits of a merge request.
func (c *Client) MRCommits(ctx context.Context, project string, iid int) ([]Commit, error) {
	var cs []Commit
	return cs, c.Do(ctx, "GET", fmt.Sprintf("/projects/%s/merge_requests/%d/commits?per_page=100", P(project), iid), nil, &cs)
}

// Pipelines lists pipelines for a commit, newest first.
func (c *Client) Pipelines(ctx context.Context, project, sha string) ([]Pipeline, error) {
	var ps []Pipeline
	return ps, c.Do(ctx, "GET", "/projects/"+P(project)+"/pipelines?order_by=id&sort=desc&sha="+url.QueryEscape(sha), nil, &ps)
}

// StatusCheckResponse reports an external status check result (GitLab Ultimate).
func (c *Client) StatusCheckResponse(ctx context.Context, project string, iid int, sha string, checkID int, passed bool) error {
	status := "failed"
	if passed {
		status = "passed"
	}
	return c.Do(ctx, "POST", fmt.Sprintf("/projects/%s/merge_requests/%d/status_check_responses", P(project), iid),
		map[string]any{"sha": sha, "external_status_check_id": checkID, "status": status}, nil)
}
