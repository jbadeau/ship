package policy_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"juliusbaer.com/codeak/ship/internal/config"
	"juliusbaer.com/codeak/ship/internal/freight"
	"juliusbaer.com/codeak/ship/internal/policy"
	"juliusbaer.com/codeak/ship/internal/testutil"
)

const bundleType = "application/vnd.example.deploy-bundle.v1"

func shipYAML(host string) string {
	return fmt.Sprintf(`apiVersion: ship/v1alpha1
apps:
  SV-API_GW_MGT:
    repository: %[1]s/deploy/gw
    artifactTypes: [%[2]s]
    deploy: { driver: noop }
  other:
    repository: %[1]s/deploy/other
    deploy: { driver: noop }
stages:
  - name: DEV
  - name: TEST
    upstream: DEV
  - name: PROD
    upstream: TEST
    requiredEvidence:
      - artifactType: %[3]s
        annotations: { ship.dev/kind: integration-test, ship.dev/stage: TEST, ship.dev/result: pass }
`, host, bundleType, freight.EvidenceArtifactType)
}

type fixture struct {
	t    *testing.T
	host string
	c    policy.Checker
	base string
}

func setup(t *testing.T) *fixture {
	host := testutil.Registry(t)
	repo := testutil.GitRepo(t)
	base := testutil.Commit(t, repo, "init", map[string]string{"ship.yaml": shipYAML(host)})
	cfg, err := config.Parse([]byte(shipYAML(host)))
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, host: host, base: base,
		c: policy.Checker{Cfg: cfg, Repo: repo, Reg: freight.NewOCI([]string{host})}}
}

// propose commits files on a throwaway branch from base, validates base..head, and returns violations.
func (f *fixture) propose(files map[string]string) []string {
	f.t.Helper()
	r := f.c.Repo
	testutil.Must(f.t, r, "checkout", "-q", "-B", "mr", f.base)
	head := testutil.Commit(f.t, r, "mr", files)
	testutil.Must(f.t, r, "checkout", "-q", "main")
	rep, err := f.c.Validate(context.Background(), f.base, head)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for _, it := range rep.Items {
		out = append(out, it.Violations...)
	}
	return out
}

// merge lands files on main and makes it the new base.
func (f *fixture) merge(files map[string]string) {
	testutil.Must(f.t, f.c.Repo, "checkout", "-q", "main")
	f.base = testutil.Commit(f.t, f.c.Repo, "merge", files)
}

func (f *fixture) evidence(ref string, ann map[string]string) {
	f.t.Helper()
	if _, err := f.c.Reg.PushEvidence(context.Background(), ref, freight.EvidenceArtifactType, ann, nil); err != nil {
		f.t.Fatal(err)
	}
}

func pin(ref string) string { return "freight: " + ref + "\n" }

func expect(t *testing.T, got []string, rule string) {
	t.Helper()
	if rule == "" {
		if len(got) != 0 {
			t.Fatalf("expected pass, got %v", got)
		}
		return
	}
	for _, v := range got {
		if strings.HasPrefix(v, rule) {
			return
		}
	}
	t.Fatalf("expected a %s violation, got %v", rule, got)
}

func TestProgression(t *testing.T) {
	f := setup(t)
	a := testutil.PushArtifact(t, f.host+"/deploy/gw", "1.0.0", bundleType, nil)

	expect(t, f.propose(map[string]string{"ledger/SV-API_GW_MGT/DEV.yaml": pin(a)}), "")
	// Skipping TEST is rejected, even in the same MR as DEV (history is judged at base).
	expect(t, f.propose(map[string]string{"ledger/SV-API_GW_MGT/TEST.yaml": pin(a)}), "V5")
	expect(t, f.propose(map[string]string{
		"ledger/SV-API_GW_MGT/DEV.yaml": pin(a), "ledger/SV-API_GW_MGT/TEST.yaml": pin(a)}), "V5")

	f.merge(map[string]string{"ledger/SV-API_GW_MGT/DEV.yaml": pin(a)})
	// Pinned in DEV but not (yet) deployed there: not eligible.
	expect(t, f.propose(map[string]string{"ledger/SV-API_GW_MGT/TEST.yaml": pin(a)}), "V5")
	ctx := context.Background()
	// Evidence for the wrong stage, or a failed deploy, does not count.
	f.evidence(a, map[string]string{"ship.dev/kind": "deployed", "ship.dev/stage": "PROD", "ship.dev/result": "pass"})
	f.evidence(a, map[string]string{"ship.dev/kind": "deployed", "ship.dev/stage": "DEV", "ship.dev/result": "fail"})
	expect(t, f.propose(map[string]string{"ledger/SV-API_GW_MGT/TEST.yaml": pin(a)}), "V5")
	f.evidence(a, policy.DeployedEvidence("DEV"))
	expect(t, f.propose(map[string]string{"ledger/SV-API_GW_MGT/TEST.yaml": pin(a)}), "")

	// Deployed evidence alone is not enough: the ledger must show the upstream pin.
	c := testutil.PushArtifact(t, f.host+"/deploy/gw", "0.9.0", bundleType, nil)
	f.evidence(c, policy.DeployedEvidence("DEV"))
	expect(t, f.propose(map[string]string{"ledger/SV-API_GW_MGT/TEST.yaml": pin(c)}), "V5")

	// "Was ever pinned": a newer DEV pin does not revoke A's eligibility.
	b := testutil.PushArtifact(t, f.host+"/deploy/gw", "1.1.0", bundleType, nil)
	f.merge(map[string]string{"ledger/SV-API_GW_MGT/DEV.yaml": pin(b)})
	expect(t, f.propose(map[string]string{"ledger/SV-API_GW_MGT/TEST.yaml": pin(a)}), "")
	f.merge(map[string]string{"ledger/SV-API_GW_MGT/TEST.yaml": pin(a)})

	// The drift case: pinned in TEST but the deploy failed or was refused.
	expect(t, f.propose(map[string]string{"ledger/SV-API_GW_MGT/PROD.yaml": pin(a)}), "V5")
	f.evidence(a, policy.DeployedEvidence("TEST"))

	// PROD needs evidence.
	expect(t, f.propose(map[string]string{"ledger/SV-API_GW_MGT/PROD.yaml": pin(a)}), "V6")
	// Wrong stage in evidence does not count.
	if _, err := f.c.Reg.PushEvidence(ctx, a, freight.EvidenceArtifactType,
		map[string]string{"ship.dev/kind": "integration-test", "ship.dev/stage": "DEV", "ship.dev/result": "pass"}, nil); err != nil {
		t.Fatal(err)
	}
	expect(t, f.propose(map[string]string{"ledger/SV-API_GW_MGT/PROD.yaml": pin(a)}), "V6")
	if _, err := f.c.Reg.PushEvidence(ctx, a, freight.EvidenceArtifactType,
		map[string]string{"ship.dev/kind": "integration-test", "ship.dev/stage": "TEST", "ship.dev/result": "pass"}, []byte(`{"tests":42}`)); err != nil {
		t.Fatal(err)
	}
	expect(t, f.propose(map[string]string{"ledger/SV-API_GW_MGT/PROD.yaml": pin(a)}), "")
}

func TestRejections(t *testing.T) {
	f := setup(t)
	a := testutil.PushArtifact(t, f.host+"/deploy/gw", "1.0.0", bundleType, nil)
	o := testutil.PushArtifact(t, f.host+"/deploy/other", "1.0.0", "", nil)
	wrongType := testutil.PushArtifact(t, f.host+"/deploy/gw", "x", "application/vnd.oci.image.config.v1+json", nil)
	f.merge(map[string]string{"ledger/SV-API_GW_MGT/DEV.yaml": pin(a)})

	cases := []struct {
		name  string
		files map[string]string
		rule  string
	}{
		{"unknown app", map[string]string{"ledger/nope/DEV.yaml": pin(a)}, "V1"},
		{"unknown stage", map[string]string{"ledger/SV-API_GW_MGT/QA.yaml": pin(a)}, "V1"},
		{"bad path", map[string]string{"ledger/SV-API_GW_MGT/x/DEV.yaml": pin(a)}, "V1"},
		{"delete", map[string]string{"ledger/SV-API_GW_MGT/DEV.yaml": ""}, "V1"},
		{"tag", map[string]string{"ledger/SV-API_GW_MGT/DEV.yaml": pin(f.host + "/deploy/gw:1.0.0")}, "V2"},
		{"unknown field", map[string]string{"ledger/SV-API_GW_MGT/DEV.yaml": pin(a) + "approvedBy: me\n"}, "V2"},
		{"other app's repo", map[string]string{"ledger/SV-API_GW_MGT/DEV.yaml": pin(o)}, "V3"},
		{"missing digest", map[string]string{"ledger/SV-API_GW_MGT/DEV.yaml": pin(f.host + "/deploy/gw@sha256:" + strings.Repeat("a", 64))}, "V4"},
		{"artifact type", map[string]string{"ledger/SV-API_GW_MGT/DEV.yaml": pin(wrongType)}, "V4"},
		{"policy edit in promotion", map[string]string{"ledger/other/DEV.yaml": pin(o), "ship.yaml": shipYAML(f.host) + "# weaken\n"}, "V7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { expect(t, f.propose(tc.files), tc.rule) })
	}
	// A policy-only MR is not a promotion: no pin changes, nothing to judge here.
	expect(t, f.propose(map[string]string{"ship.yaml": shipYAML(f.host) + "# comment\n"}), "")
}

func TestUpstreamSoak(t *testing.T) {
	f := setup(t)
	y := strings.Replace(shipYAML(f.host), "    upstream: DEV\n", "    upstream: DEV\n    upstreamSoak: 24h\n", 1)
	cfg, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	f.c.Cfg = cfg
	now := time.Now()
	f.c.Now = func() time.Time { return now }
	a := testutil.PushArtifact(t, f.host+"/deploy/gw", "1.0.0", bundleType, nil)
	f.merge(map[string]string{"ledger/SV-API_GW_MGT/DEV.yaml": pin(a)})

	// Undated deployed evidence cannot prove soak: fail closed.
	undated := policy.DeployedEvidence("DEV")
	undated[freight.CreatedAnnotation] = "not-a-time"
	f.evidence(a, undated)
	expect(t, f.propose(map[string]string{"ledger/SV-API_GW_MGT/TEST.yaml": pin(a)}), "V5: "+a[len(a)-64:][:12]+": deployed/DEV evidence has no")

	// Deployed 2h ago (ship stamps the time): too fresh.
	dated := policy.DeployedEvidence("DEV")
	dated[freight.CreatedAnnotation] = now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	f.evidence(a, dated)
	got := f.propose(map[string]string{"ledger/SV-API_GW_MGT/TEST.yaml": pin(a)})
	expect(t, got, "V5")
	if !strings.Contains(strings.Join(got, ""), "eligible after") {
		t.Fatalf("expected eligibility time, got %v", got)
	}
	// 25h later it is eligible; the earliest deploy counts, so a redeploy does not reset the clock.
	now = now.Add(23 * time.Hour)
	f.evidence(a, policy.DeployedEvidence("DEV")) // fresh redeploy, stamped with real time.Now()
	expect(t, f.propose(map[string]string{"ledger/SV-API_GW_MGT/TEST.yaml": pin(a)}), "")
}

func TestSoakRequiresUpstream(t *testing.T) {
	y := strings.Replace(shipYAML("r.example"), "  - name: DEV\n", "  - name: DEV\n    upstreamSoak: 1h\n", 1)
	if _, err := config.Parse([]byte(y)); err == nil || !strings.Contains(err.Error(), "requires upstream") {
		t.Fatalf("expected upstream error, got %v", err)
	}
	y = strings.Replace(shipYAML("r.example"), "    upstream: DEV\n", "    upstream: DEV\n    upstreamSoak: soon\n", 1)
	if _, err := config.Parse([]byte(y)); err == nil {
		t.Fatal("expected invalid duration error")
	}
}
