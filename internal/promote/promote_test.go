package promote_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"juliusbaer.com/codeak/ship/internal/config"
	"juliusbaer.com/codeak/ship/internal/forge"
	"juliusbaer.com/codeak/ship/internal/freight"
	"juliusbaer.com/codeak/ship/internal/gitx"
	"juliusbaer.com/codeak/ship/internal/policy"
	"juliusbaer.com/codeak/ship/internal/promote"
	"juliusbaer.com/codeak/ship/internal/testutil"
)

func setup(t *testing.T) (policy.Checker, string, gitx.Repo) {
	host := testutil.Registry(t)
	y := fmt.Sprintf("apiVersion: ship/v1alpha1\napps:\n  gw:\n    repository: %s/deploy/gw\n    deploy: { driver: noop }\nstages:\n  - name: DEV\n", host)
	cfg, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	repo := testutil.GitRepo(t)
	testutil.Commit(t, repo, "init", map[string]string{"ship.yaml": y})
	remote := gitx.Repo{Dir: t.TempDir()}
	testutil.Must(t, remote, "init", "-q", "--bare")
	testutil.Must(t, repo, "remote", "add", "origin", remote.Dir)
	ref := testutil.PushArtifact(t, host+"/deploy/gw", "1", "", nil)
	return policy.Checker{Cfg: cfg, Repo: repo, Reg: freight.NewOCI([]string{host})}, ref, remote
}

func TestPushOpensCRThroughForgeAPI(t *testing.T) {
	c, ref, remote := setup(t)
	var got forge.CRSpec
	res, err := promote.Run(context.Background(), c, promote.Options{
		App: "gw", Stage: "DEV", Freight: ref, Reason: "r", RequestedBy: "alice", Push: true,
		OpenCR: func(_ context.Context, s forge.CRSpec) (forge.ChangeRequest, error) {
			got = s
			return forge.ChangeRequest{ID: 1, URL: "https://forge/cr/1"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Pushed || res.CR == nil || res.CR.URL != "https://forge/cr/1" {
		t.Fatalf("%+v", res)
	}
	if got.SourceBranch != res.Branch || got.TargetBranch != "main" || !got.RemoveSourceBranch ||
		len(got.Labels) != 1 || got.Labels[0] != "promotion" || !strings.HasPrefix(got.Title, "Promote gw to DEV") ||
		!strings.Contains(got.Description, "Requested-by: alice") {
		t.Fatalf("spec: %+v", got)
	}
	// The branch was pushed before the CR was opened.
	testutil.Must(t, remote, "rev-parse", "--verify", "refs/heads/"+res.Branch)
}

func TestPushReportsCRFailureAfterPush(t *testing.T) {
	c, ref, _ := setup(t)
	res, err := promote.Run(context.Background(), c, promote.Options{App: "gw", Stage: "DEV", Freight: ref, Push: true,
		OpenCR: func(context.Context, forge.CRSpec) (forge.ChangeRequest, error) {
			return forge.ChangeRequest{}, fmt.Errorf("403")
		}})
	if err == nil || !res.Pushed || !strings.Contains(err.Error(), "pushed "+res.Branch) {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}
