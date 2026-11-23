// Package forgetest is the shared contract suite every forge adapter must pass
// (docs/adapters.md §2.5). Adapters run it against a real or sandboxed forge:
//
//	forgetest.Run(t, forgetest.Target{Forge: f, Repo: "contract/forgetest"})
//
// The repo must exist, have a default branch, and the client must be allowed to
// push branches and open CRs. The suite only creates branches under
// "forgetest/<run>/" and closes every CR it opens.
//
// Not covered here, because they need forge-specific setup: Approve (needs a
// second identity and approval rules), AutoMerge and Runs (need CI), and
// ReportCheck (GitLab Ultimate external status check / GitHub App). Those are
// exercised by the e2e scenario.
package forgetest

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"juliusbaer.com/codeak/ship/internal/forge"
)

type Target struct {
	Forge forge.Forge
	Repo  string // path of a scratch repo
	// Group, if set, must contain Repo; GroupCRs is then checked too.
	Group string
}

// Run executes the contract suite.
func Run(t *testing.T, tg Target) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	f, repoPath := tg.Forge, tg.Repo
	run := fmt.Sprintf("%d", time.Now().UnixNano())
	branch := "forgetest/" + run + "/change"
	file := "forgetest/" + run + ".txt"

	must := func(err error, what string) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}

	me, err := f.Whoami(ctx)
	must(err, "Whoami")
	if me == "" {
		t.Fatal("Whoami returned an empty identity")
	}
	if u, p, err := f.GitCredentials(ctx); err != nil || u == "" || p == "" {
		t.Fatalf("GitCredentials: user=%q, password set=%v, err=%v", u, p != "", err)
	}

	repo, err := f.Repo(ctx, repoPath)
	must(err, "Repo")
	if repo.Path != repoPath || repo.DefaultBranch == "" || repo.CloneURL == "" {
		t.Fatalf("Repo: %+v", repo)
	}
	if repo.ID != "" {
		byID, err := f.Repo(ctx, repo.ID)
		must(err, "Repo by id")
		if byID.Path != repo.Path {
			t.Fatalf("Repo(%s).Path = %q, want %q", repo.ID, byID.Path, repo.Path)
		}
	}
	if _, err := f.Repo(ctx, repoPath+"-does-not-exist"); !forge.IsNotFound(err) {
		t.Fatalf("Repo(missing): want ErrNotFound, got %v", err)
	}

	mainSHA, ok, err := f.BranchSHA(ctx, repoPath, repo.DefaultBranch)
	must(err, "BranchSHA")
	if !ok || len(mainSHA) < 40 {
		t.Fatalf("BranchSHA(default) = %q, %v", mainSHA, ok)
	}
	if _, ok, err := f.BranchSHA(ctx, repoPath, branch); err != nil || ok {
		t.Fatalf("BranchSHA(missing) = %v, %v", ok, err)
	}
	if _, ok, err := f.File(ctx, repoPath, file, repo.DefaultBranch); err != nil || ok {
		t.Fatalf("File(missing) = %v, %v", ok, err)
	}

	// Commit creates the branch from StartBranch.
	msg := "forgetest " + run + "\n\nRequested-by: forgetest\n"
	must(f.Commit(ctx, repoPath, forge.CommitSpec{Branch: branch, StartBranch: repo.DefaultBranch, Message: msg,
		Files: []forge.FileChange{{Path: file, Content: "one\n", Create: true}}}), "Commit create")
	must(f.Commit(ctx, repoPath, forge.CommitSpec{Branch: branch, Message: "forgetest update " + run,
		Files: []forge.FileChange{{Path: file, Content: "two\n"}}}), "Commit update")
	head, ok, err := f.BranchSHA(ctx, repoPath, branch)
	must(err, "BranchSHA(branch)")
	if !ok || head == mainSHA {
		t.Fatalf("branch head %q (main %q)", head, mainSHA)
	}
	b, ok, err := f.File(ctx, repoPath, file, branch)
	must(err, "File")
	if !ok || string(b) != "two\n" {
		t.Fatalf("File = %q, %v", b, ok)
	}
	if b, ok, _ := f.File(ctx, repoPath, file, head); !ok || string(b) != "two\n" {
		t.Fatalf("File at SHA = %q, %v", b, ok)
	}
	if anc, err := f.IsAncestor(ctx, repoPath, mainSHA, head); err != nil || !anc {
		t.Fatalf("IsAncestor(main, head) = %v, %v", anc, err)
	}
	if anc, err := f.IsAncestor(ctx, repoPath, head, mainSHA); err != nil || anc {
		t.Fatalf("IsAncestor(head, main) = %v, %v", anc, err)
	}

	// Change request lifecycle.
	label := "forgetest"
	cr, err := f.OpenCR(ctx, repoPath, forge.CRSpec{SourceBranch: branch, TargetBranch: repo.DefaultBranch,
		Title: "forgetest " + run, Description: "Freight: x\nLedger-MR: 1\n", Labels: []string{label}, RemoveSourceBranch: true})
	must(err, "OpenCR")
	defer func() { _ = f.CloseCR(context.Background(), repoPath, cr.ID, "") }()
	if cr.ID == 0 || cr.State != forge.StateOpen || cr.SourceBranch != branch || cr.URL == "" {
		t.Fatalf("OpenCR: %+v", cr)
	}

	got := waitCR(t, ctx, f, repoPath, cr.ID, func(c forge.ChangeRequest) bool { return c.HeadSHA == head && c.BaseSHA != "" })
	if got.Author != me || got.TargetBranch != repo.DefaultBranch || !strings.Contains(got.Description, "Ledger-MR: 1") ||
		!slices.Contains(got.Labels, label) || got.BaseSHA != mainSHA || len(got.MergeSHAs) != 0 {
		t.Fatalf("CR: %+v (want author %s, base %s)", got, me, mainSHA)
	}

	list, err := f.ListCRs(ctx, repoPath, forge.CRQuery{SourceBranch: branch})
	must(err, "ListCRs")
	if len(list) != 1 || list[0].ID != cr.ID {
		t.Fatalf("ListCRs(source) = %d CRs", len(list))
	}
	list, err = f.ListCRs(ctx, repoPath, forge.CRQuery{State: forge.StateOpen, Label: label})
	must(err, "ListCRs(label)")
	if !slices.ContainsFunc(list, func(c forge.ChangeRequest) bool { return c.ID == cr.ID }) {
		t.Fatal("ListCRs(label) misses the CR")
	}
	if tg.Group != "" {
		gl, err := f.GroupCRs(ctx, tg.Group, forge.CRQuery{State: forge.StateOpen})
		must(err, "GroupCRs")
		i := slices.IndexFunc(gl, func(c forge.ChangeRequest) bool { return c.ID == cr.ID && c.RepoID == repo.ID })
		if i < 0 {
			t.Fatal("GroupCRs misses the CR (or RepoID is not set)")
		}
	}

	diffs, err := f.Diffs(ctx, repoPath, cr.ID)
	must(err, "Diffs")
	if len(diffs) != 1 || diffs[0].NewPath != file || !diffs[0].Created || diffs[0].Deleted || diffs[0].Renamed {
		t.Fatalf("Diffs = %+v", diffs)
	}
	commits, err := f.CRCommits(ctx, repoPath, cr.ID)
	must(err, "CRCommits")
	if len(commits) != 2 || !slices.ContainsFunc(commits, func(c forge.Commit) bool { return strings.Contains(c.Message, "Requested-by: forgetest") }) {
		t.Fatalf("CRCommits = %+v", commits)
	}
	if by, err := f.Approvers(ctx, repoPath, cr.ID); err != nil || len(by) != 0 {
		t.Fatalf("Approvers of a fresh CR = %v, %v", by, err)
	}
	must(f.Comment(ctx, repoPath, cr.ID, "forgetest comment"), "Comment")
	if _, err := f.CR(ctx, repoPath, 999999999); !forge.IsNotFound(err) {
		t.Fatalf("CR(missing): want ErrNotFound, got %v", err)
	}

	must(f.CloseCR(ctx, repoPath, cr.ID, "forgetest: closing"), "CloseCR")
	closed := waitCR(t, ctx, f, repoPath, cr.ID, func(c forge.ChangeRequest) bool { return c.State == forge.StateClosed })
	if closed.State != forge.StateClosed {
		t.Fatalf("state after close: %s", closed.State)
	}
	if open, _ := f.ListCRs(ctx, repoPath, forge.CRQuery{SourceBranch: branch}); len(open) != 0 {
		t.Fatal("closed CR still listed as open")
	}
	if all, _ := f.ListCRs(ctx, repoPath, forge.CRQuery{State: forge.StateAll, SourceBranch: branch}); len(all) != 1 {
		t.Fatalf("ListCRs(all) after close = %d", len(all))
	}

	must(f.DeleteBranch(ctx, repoPath, branch), "DeleteBranch")
	if _, ok, err := f.BranchSHA(ctx, repoPath, branch); err != nil || ok {
		t.Fatalf("branch still exists after delete: %v %v", ok, err)
	}
}

// waitCR polls until cond holds: forges compute some CR fields asynchronously.
func waitCR(t *testing.T, ctx context.Context, f forge.Forge, repo string, id int, cond func(forge.ChangeRequest) bool) forge.ChangeRequest {
	t.Helper()
	for {
		c, err := f.CR(ctx, repo, id)
		if err != nil {
			t.Fatalf("CR: %v", err)
		}
		if cond(c) {
			return c
		}
		select {
		case <-ctx.Done():
			return c
		case <-time.After(500 * time.Millisecond):
		}
	}
}
