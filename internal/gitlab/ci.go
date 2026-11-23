package gitlab

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"juliusbaer.com/codeak/ship/internal/forge"
	"juliusbaer.com/codeak/ship/internal/secret"
)

// DetectCI reads GitLab CI's predefined variables. ok is false outside GitLab CI.
func DetectCI(lookup func(string) string) (forge.CI, bool) {
	if lookup("GITLAB_CI") != "true" && lookup("CI_SERVER_URL") == "" {
		return forge.CI{}, false
	}
	iid, _ := strconv.Atoi(lookup("CI_MERGE_REQUEST_IID"))
	return forge.CI{
		Forge:     Name,
		ServerURL: lookup("CI_SERVER_URL"),
		Repo:      lookup("CI_PROJECT_PATH"),
		CR:        iid,
		BaseSHA:   lookup("CI_MERGE_REQUEST_DIFF_BASE_SHA"),
		BeforeSHA: lookup("CI_COMMIT_BEFORE_SHA"),
		RunURL:    lookup("CI_PIPELINE_URL"),
	}, true
}

// Webhook verifies GitLab's X-Gitlab-Token header and parses merge request events.
type Webhook struct {
	Secret secret.Source
	// BranchPrefix limits relevant events to CRs from these branches (e.g. "ship/").
	BranchPrefix string
}

var _ forge.Webhook = Webhook{}

var errBadToken = errors.New("invalid webhook token")

func (w Webhook) Verify(r *http.Request, _ []byte) error {
	want, err := w.Secret(r.Context())
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Gitlab-Token")), []byte(want.Reveal())) != 1 {
		return errBadToken
	}
	return nil
}

// hook is the subset of GitLab's merge request event used as a trigger.
type hook struct {
	ObjectKind string `json:"object_kind"`
	Project    struct {
		ID int `json:"id"`
	} `json:"project"`
	Attrs struct {
		IID          int    `json:"iid"`
		SourceBranch string `json:"source_branch"`
		State        string `json:"state"`
		Action       string `json:"action"`
	} `json:"object_attributes"`
}

// Parse returns the project id (not the path: the payload is untrusted, the
// caller resolves the id through the API) and MR iid.
func (w Webhook) Parse(body []byte) (string, int, bool, error) {
	var h hook
	if err := json.Unmarshal(body, &h); err != nil {
		return "", 0, false, err
	}
	if !Relevant(h.ObjectKind, h.Attrs.SourceBranch, h.Attrs.State, h.Attrs.Action, w.BranchPrefix) || h.Project.ID == 0 || h.Attrs.IID == 0 {
		return "", 0, false, nil
	}
	return strconv.Itoa(h.Project.ID), h.Attrs.IID, true, nil
}

// Relevant filters webhook events cheaply. A wrong answer only costs (or skips)
// a re-check: the approver's sweep catches anything skipped.
func Relevant(kind, branch, state, action, prefix string) bool {
	if kind != "merge_request" || !strings.HasPrefix(branch, prefix) || state != "opened" {
		return false
	}
	switch action {
	case "open", "reopen", "update":
		return true
	}
	return false
}
