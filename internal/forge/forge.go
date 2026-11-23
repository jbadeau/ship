// Package forge is ship's view of a git forge (GitLab, GitHub, …), in neutral
// terms: repos, change requests (merge/pull requests), reviews, comments and a
// pre-merge check. Adapters live in their own packages (internal/gitlab) and are
// selected in internal/forge/forges. See docs/adapters.md §2.
//
// Governance (protected branches, approval rules, compliance frameworks) is
// deliberately not abstracted: it is configured per forge (docs/setup.md).
package forge

import (
	"context"
	"errors"
	"net/http"
)

// Change request states.
const (
	StateOpen   = "open"
	StateClosed = "closed"
	StateMerged = "merged"
	StateAll    = "all" // query only
)

// Run (pipeline / workflow run) statuses ship acts on. Adapters map their own
// statuses onto these where they mean the same; others pass through unchanged
// and are treated as "still going".
const (
	RunSuccess  = "success"
	RunFailed   = "failed"
	RunCanceled = "canceled"
	RunSkipped  = "skipped"
)

// ErrNotFound is returned (wrapped) when a repo, CR or ref does not exist.
var ErrNotFound = errors.New("not found")

func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

type Repo struct {
	ID            string // forge-internal id, if any (GitLab project id)
	Path          string // group/name or owner/name
	DefaultBranch string
	WebURL        string
	CloneURL      string // HTTPS, without credentials
}

type Run struct {
	ID     int
	Status string
	URL    string
}

// ChangeRequest is a GitLab merge request or a GitHub pull request.
type ChangeRequest struct {
	ID           int    // per-repo number (GitLab iid, GitHub number)
	RepoID       string // forge-internal repo id, when listing across repos
	Title        string
	Description  string
	State        string // StateOpen, StateClosed, StateMerged
	SourceBranch string
	TargetBranch string
	HeadSHA      string
	BaseSHA      string   // diff base
	MergeSHAs    []string // commits the merge may have produced (merge, squash); empty until merged
	Author       string
	Labels       []string
	URL          string
	AutoMerge    bool // merge-when-checks-pass is set
	HasConflicts bool
	Pipeline     *Run // latest run on the head, if the forge reports one
}

type FileDiff struct {
	OldPath, NewPath          string
	Created, Renamed, Deleted bool
}

type Commit struct {
	SHA, Message string
}

type FileChange struct {
	Path    string
	Content string
	Create  bool // false = update an existing file
}

type CommitSpec struct {
	Branch      string // branch to commit to (created from StartBranch if missing)
	StartBranch string
	Message     string
	Files       []FileChange
}

type CRSpec struct {
	SourceBranch, TargetBranch string
	Title, Description         string
	Labels                     []string
	RemoveSourceBranch         bool
}

type CRQuery struct {
	State        string // StateOpen (default), StateClosed, StateMerged, StateAll
	SourceBranch string
	TargetBranch string
	Label        string
}

// Check is the pre-merge result ship reports on a ledger CR (V8).
type Check struct {
	// Key identifies the check on the forge: the external status check id on
	// GitLab, the check-run name on GitHub.
	Key     string
	Passed  bool
	Summary string
}

// Forge is everything ship does on a forge.
type Forge interface {
	Name() string

	// Who the client is authenticated as.
	Whoami(ctx context.Context) (string, error)
	// GitCredentials returns HTTPS basic-auth credentials for git clone/fetch.
	GitCredentials(ctx context.Context) (user, password string, err error)

	// Repo content. ref is a branch, tag or SHA.
	Repo(ctx context.Context, repo string) (Repo, error) // repo = path or forge id
	File(ctx context.Context, repo, path, ref string) ([]byte, bool, error)
	BranchSHA(ctx context.Context, repo, branch string) (string, bool, error)
	DeleteBranch(ctx context.Context, repo, branch string) error
	IsAncestor(ctx context.Context, repo, commit, ref string) (bool, error)
	Commit(ctx context.Context, repo string, c CommitSpec) error

	// Change requests. Lists are newest first and complete (adapters paginate).
	CR(ctx context.Context, repo string, id int) (ChangeRequest, error)
	ListCRs(ctx context.Context, repo string, q CRQuery) ([]ChangeRequest, error)
	// GroupCRs lists CRs across a group / organisation; RepoID is set on each.
	GroupCRs(ctx context.Context, group string, q CRQuery) ([]ChangeRequest, error)
	OpenCR(ctx context.Context, repo string, s CRSpec) (ChangeRequest, error)
	CloseCR(ctx context.Context, repo string, id int, comment string) error
	Comment(ctx context.Context, repo string, id int, body string) error
	Diffs(ctx context.Context, repo string, id int) ([]FileDiff, error)
	CRCommits(ctx context.Context, repo string, id int) ([]Commit, error)
	CRsForCommit(ctx context.Context, repo, sha string) ([]ChangeRequest, error)
	Approvers(ctx context.Context, repo string, id int) ([]string, error)
	// Approve approves exactly head sha; the forge must refuse if the CR moved.
	Approve(ctx context.Context, repo string, id int, sha string) error
	// AutoMerge merges sha once required checks pass.
	AutoMerge(ctx context.Context, repo string, id int, sha string) error

	// Runs lists pipeline/workflow runs for a commit, newest first.
	Runs(ctx context.Context, repo, sha string) ([]Run, error)
	ReportCheck(ctx context.Context, repo string, id int, sha string, c Check) error
}

// CI is what ship needs from the CI system running it.
type CI struct {
	Forge     string // adapter name, e.g. "gitlab"
	ServerURL string
	Repo      string // path of the repo the job runs for
	CR        int    // CR number in CR pipelines, else 0
	BaseSHA   string // CR diff base
	BeforeSHA string // previous head of the branch on push pipelines
	RunURL    string
}

// Webhook authenticates and parses forge webhooks. The payload is a trigger
// only and is not trusted: callers re-read the CR through the API.
type Webhook interface {
	Verify(r *http.Request, body []byte) error
	// Parse returns the repo (path or forge id) and CR of a relevant CR event.
	Parse(body []byte) (repo string, cr int, relevant bool, err error)
}
