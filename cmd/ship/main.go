// Command ship: promotion as merge requests. See docs/design.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jbadeau/ship/internal/approval"
	"github.com/jbadeau/ship/internal/config"
	"github.com/jbadeau/ship/internal/deploy"
	"github.com/jbadeau/ship/internal/driver"
	"github.com/jbadeau/ship/internal/driver/builtin"
	"github.com/jbadeau/ship/internal/envmr"
	"github.com/jbadeau/ship/internal/freight"
	"github.com/jbadeau/ship/internal/gitlab"
	"github.com/jbadeau/ship/internal/gitx"
	"github.com/jbadeau/ship/internal/ledger"
	"github.com/jbadeau/ship/internal/policy"
	"github.com/jbadeau/ship/internal/promote"
)

var version = "dev"

const usage = `ship — promotion as merge requests

Usage: ship <command> [flags]

Propose
  promote        create a promotion branch/commit (and MR with --push)
Read
  status         app × stage matrix of pinned Freight
  history        pin history of one app/stage
  evidence       list evidence attached to a Freight
CI: ledger repo
  validate       fail-closed policy check of an MR (rules from --base)
  plan           driver dry-run for changed pins
  deploy         hand changed pins to their drivers
  attest         attach evidence to a Freight
CI: env repo
  verify-env-mr  provenance check; approves ship MRs as ship-approver
Kubernetes
  controller     ship-controller: Promotion CRs -> ledger MRs (+ V8 status check)

Run 'ship <command> -h' for flags.
`

type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit %d", e.code) }

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		fmt.Print(usage)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cmds := map[string]func(context.Context, []string) error{
		"promote": cmdPromote, "status": cmdStatus, "history": cmdHistory, "evidence": cmdEvidence,
		"validate": cmdValidate, "plan": cmdPlan, "deploy": cmdDeploy, "attest": cmdAttest,
		"verify-env-mr": cmdVerifyEnvMR, "controller": cmdController,
		"version": func(context.Context, []string) error { fmt.Println(version); return nil },
	}
	fn, ok := cmds[os.Args[1]]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err := fn(ctx, os.Args[2:]); err != nil {
		var ee exitError
		if errors.As(err, &ee) {
			os.Exit(ee.code)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// ---- shared setup ----

type common struct {
	repo   string
	config string
}

func (c *common) bind(fs *flag.FlagSet) {
	fs.StringVar(&c.repo, "repo", ".", "path to the ledger repo")
	fs.StringVar(&c.config, "config", "ship.yaml", "policy file path inside the repo")
}

// load reads ship.yaml at rev ("" = working tree).
func (c *common) load(rev string) (*config.File, gitx.Repo, error) {
	repo := gitx.Repo{Dir: c.repo}
	var b []byte
	var err error
	if rev == "" {
		b, err = os.ReadFile(c.repo + "/" + c.config)
	} else {
		var found bool
		b, found, err = repo.Show(rev, c.config)
		if err == nil && !found {
			err = fmt.Errorf("%s not found at %s", c.config, rev)
		}
	}
	if err != nil {
		return nil, repo, err
	}
	cfg, err := config.Parse(b)
	return cfg, repo, err
}

func registry(cfg *config.File) *freight.OCI {
	var insecure []string
	if cfg != nil {
		insecure = cfg.InsecureRegistries()
	}
	insecure = append(insecure, strings.Split(os.Getenv("SHIP_INSECURE_REGISTRIES"), ",")...)
	return freight.NewOCI(insecure)
}

func gitlabClient(tokenEnv string) *gitlab.Client {
	u := os.Getenv("SHIP_GITLAB_URL")
	if u == "" {
		u = os.Getenv("CI_SERVER_URL")
	}
	tok := os.Getenv(tokenEnv)
	if u == "" || tok == "" {
		return nil
	}
	return gitlab.New(u, tok)
}

func zeroSHA(s string) bool { return strings.Trim(s, "0") == "" }

func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return exitError{2}
	}
	return nil
}

// ---- promote ----

func cmdPromote(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("promote", flag.ContinueOnError)
	var c common
	c.bind(fs)
	var o promote.Options
	fs.StringVar(&o.App, "app", "", "app name (required)")
	fs.StringVar(&o.Stage, "stage", "", "target stage (required)")
	fs.StringVar(&o.Freight, "freight", "", "registry/repo@sha256:… or sha256:… (required)")
	fs.StringVar(&o.Reason, "reason", "", "why (goes into the commit and MR)")
	fs.StringVar(&o.RequestedBy, "requested-by", os.Getenv("SHIP_REQUESTED_BY"), "person or agent identity")
	fs.BoolVar(&o.Push, "push", false, "push and create the MR via GitLab push options")
	fs.StringVar(&o.Remote, "remote", "origin", "git remote")
	fs.StringVar(&o.Target, "target", "main", "MR target branch")
	if err := parse(fs, args); err != nil {
		return err
	}
	if o.App == "" || o.Stage == "" || o.Freight == "" {
		fs.Usage()
		return exitError{2}
	}
	cfg, repo, err := c.load("HEAD")
	if err != nil {
		return err
	}
	res, err := promote.Run(ctx, policy.Checker{Cfg: cfg, Repo: repo, Reg: registry(cfg)}, o)
	if err != nil {
		return err
	}
	fmt.Printf("branch %s: %s/%s -> %s\n", res.Branch, o.App, o.Stage, res.Freight.Ref)
	if res.Pushed {
		fmt.Println("pushed; GitLab created the merge request")
	} else {
		fmt.Printf("not pushed; run: git push %s %s (or rerun with --push)\n", o.Remote, res.Branch)
	}
	return nil
}

// ---- status / history / evidence ----

func cmdStatus(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	var c common
	c.bind(fs)
	rev := fs.String("rev", "HEAD", "ledger revision")
	if err := parse(fs, args); err != nil {
		return err
	}
	cfg, repo, err := c.load(*rev)
	if err != nil {
		return err
	}
	apps := make([]string, 0, len(cfg.Apps))
	for a := range cfg.Apps {
		apps = append(apps, a)
	}
	sort.Strings(apps)
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprint(tw, "APP\tDRIVER")
	for _, s := range cfg.Stages {
		fmt.Fprint(tw, "\t"+strings.ToUpper(s.Name))
	}
	fmt.Fprintln(tw)
	for _, a := range apps {
		fmt.Fprintf(tw, "%s\t%s", a, cfg.Apps[a].Deploy.Driver)
		for _, s := range cfg.Stages {
			p, err := ledger.At(repo, *rev, cfg.LedgerDir, a, s.Name)
			switch {
			case err != nil:
				fmt.Fprint(tw, "\tINVALID")
			case p == nil:
				fmt.Fprint(tw, "\t-")
			default:
				fmt.Fprint(tw, "\t"+freight.ShortDigest(p.Digest()))
			}
		}
		fmt.Fprintln(tw)
	}
	return tw.Flush()
}

func cmdHistory(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	var c common
	c.bind(fs)
	app := fs.String("app", "", "app (required)")
	stage := fs.String("stage", "", "stage (required)")
	if err := parse(fs, args); err != nil {
		return err
	}
	cfg, repo, err := c.load("HEAD")
	if err != nil {
		return err
	}
	out, err := repo.Run("log", "--format=%H%x09%cI%x09%an%x09%(trailers:key=Requested-by,valueonly,separator=%x2C)", "HEAD", "--", ledger.Path(cfg.LedgerDir, *app, *stage))
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "COMMIT\tDATE\tAUTHOR\tREQUESTED-BY\tFREIGHT")
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 4 {
			continue
		}
		fr := "(removed)"
		if p, err := ledger.At(repo, f[0], cfg.LedgerDir, *app, *stage); err == nil && p != nil {
			fr = freight.ShortDigest(p.Digest())
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", f[0][:12], f[1], f[2], f[3], fr)
	}
	return tw.Flush()
}

func cmdEvidence(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("evidence", flag.ContinueOnError)
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: ship evidence registry/repo@sha256:…")
		return exitError{2}
	}
	refs, err := registry(nil).Referrers(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(refs)
}

// ---- validate / plan / deploy / attest ----

func cmdValidate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	var c common
	c.bind(fs)
	base := fs.String("base", os.Getenv("CI_MERGE_REQUEST_DIFF_BASE_SHA"), "MR base revision; policy is read from here")
	head := fs.String("head", "HEAD", "MR head revision")
	format := fs.String("format", "text", "text or json")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *base == "" {
		return fmt.Errorf("--base is required (CI_MERGE_REQUEST_DIFF_BASE_SHA in MR pipelines)")
	}
	cfg, repo, err := c.load(*base)
	if err != nil {
		return err
	}
	rep, err := policy.Checker{Cfg: cfg, Repo: repo, Reg: registry(cfg)}.Validate(ctx, *base, *head)
	if err != nil {
		return err
	}
	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
	} else {
		if len(rep.Items) == 0 {
			fmt.Println("no pin changes")
		}
		for _, it := range rep.Items {
			mark := "PASS"
			if len(it.Violations) > 0 {
				mark = "FAIL"
			}
			fr := ""
			if it.Freight != nil {
				fr = " -> " + it.Freight.Short()
			}
			fmt.Printf("%s %s%s\n", mark, it.Path, fr)
			for _, v := range it.Violations {
				fmt.Printf("     %s\n", v)
			}
		}
	}
	if !rep.OK() {
		return exitError{1}
	}
	return nil
}

func deployRunner(ctx context.Context, c common, rev string) (deploy.Runner, string, error) {
	cfg, repo, err := c.load(rev)
	if err != nil {
		return deploy.Runner{}, "", err
	}
	reg := registry(cfg)
	gl := gitlabClient("SHIP_GITLAB_TOKEN")
	prom := driver.Promotion{
		LedgerProject: os.Getenv("CI_PROJECT_PATH"),
		PipelineURL:   os.Getenv("CI_PIPELINE_URL"),
	}
	if prom.LedgerProject == "" {
		prom.LedgerProject = cfg.GitLab.LedgerProject
	}
	sha, err := repo.Resolve(rev)
	if err != nil {
		return deploy.Runner{}, "", err
	}
	prom.LedgerCommit = sha
	if gl != nil && prom.LedgerProject != "" {
		if mrs, err := gl.CommitMRs(ctx, prom.LedgerProject, sha); err == nil {
			for _, m := range mrs {
				if m.State == "merged" {
					prom.LedgerMR, prom.LedgerMRURL = m.IID, m.WebURL
					break
				}
			}
		}
	}
	return deploy.Runner{
		Cfg: cfg, Repo: repo, Reg: reg, Driver: builtin.Get, Promotion: prom,
		Deps: driver.Deps{GitLab: gl, Registry: reg, Log: os.Stderr},
	}, sha, nil
}

// enforceV8 is the post-merge half of V8: refuse to hand pins to drivers unless
// the ledger MR that produced this commit had independent approval.
func enforceV8(ctx context.Context, r deploy.Runner) error {
	gl := r.Deps.GitLab
	if gl == nil || r.Promotion.LedgerProject == "" {
		return fmt.Errorf("V8: cannot verify approvals (SHIP_GITLAB_TOKEN / CI_PROJECT_PATH unset); use --require-approval=false only outside CI")
	}
	if r.Promotion.LedgerMR == 0 {
		return fmt.Errorf("V8: commit %s did not come from a merged ledger MR; refusing to deploy", r.Promotion.LedgerCommit)
	}
	mr, err := gl.MR(ctx, r.Promotion.LedgerProject, r.Promotion.LedgerMR)
	if err != nil {
		return err
	}
	facts, err := approval.Gather(ctx, gl, r.Promotion.LedgerProject, mr)
	if err != nil {
		return err
	}
	res := approval.Check(r.Cfg, facts)
	if !res.OK() {
		return fmt.Errorf("ledger MR !%d: %s; refusing to deploy", mr.IID, res.Violation)
	}
	fmt.Fprintf(os.Stderr, "V8: ledger MR !%d independently approved by %s\n", mr.IID, strings.Join(res.Independent, ", "))
	return nil
}

func baseOrParent(repo gitx.Repo, base, head string) (string, error) {
	if base == "" || zeroSHA(base) {
		return repo.Resolve(head + "^")
	}
	return base, nil
}

func cmdPlan(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	var c common
	c.bind(fs)
	base := fs.String("base", os.Getenv("CI_MERGE_REQUEST_DIFF_BASE_SHA"), "base revision")
	head := fs.String("head", "HEAD", "head revision")
	if err := parse(fs, args); err != nil {
		return err
	}
	r, _, err := deployRunner(ctx, c, *head)
	if err != nil {
		return err
	}
	b, err := baseOrParent(r.Repo, *base, *head)
	if err != nil {
		return err
	}
	outs, err := r.Run(ctx, b, *head, deploy.Mode{PlanOnly: true})
	for _, o := range outs {
		fmt.Printf("== %s/%s -> %s\n", o.App, o.Stage, o.Freight)
		if o.Err != nil {
			fmt.Printf("   ERROR: %v\n", o.Err)
			continue
		}
		fmt.Printf("   %s\n", o.Plan.Summary)
		if o.Plan.Diff != "" {
			fmt.Println(o.Plan.Diff)
		}
	}
	return err
}

func cmdDeploy(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	var c common
	c.bind(fs)
	base := fs.String("base", os.Getenv("CI_COMMIT_BEFORE_SHA"), "previous revision of the default branch")
	head := fs.String("head", "HEAD", "revision to deploy")
	wait := fs.Duration("wait", 15*time.Minute, "wait for drivers to report healthy (0 = don't wait)")
	record := fs.Bool("record", true, "push 'deployed' evidence after success")
	requireApproval := fs.Bool("require-approval", true, "enforce V8 on the ledger MR that produced --head (fail closed)")
	if err := parse(fs, args); err != nil {
		return err
	}
	r, _, err := deployRunner(ctx, c, *head)
	if err != nil {
		return err
	}
	b, err := baseOrParent(r.Repo, *base, *head)
	if err != nil {
		return err
	}
	if pins, _, err := ledger.Changes(r.Repo, b, *head, r.Cfg.LedgerDir); err != nil {
		return err
	} else if len(pins) == 0 {
		fmt.Println("no pin changes to deploy")
		return nil
	}
	if *requireApproval {
		if err := enforceV8(ctx, r); err != nil {
			return err
		}
	}
	outs, err := r.Run(ctx, b, *head, deploy.Mode{Wait: *wait, Record: *record})
	if len(outs) == 0 && err == nil {
		fmt.Println("no pin changes to deploy")
	}
	for _, o := range outs {
		status := "OK"
		if o.Err != nil {
			status = "FAILED: " + o.Err.Error()
		}
		fmt.Printf("%s/%s -> %s: %s\n", o.App, o.Stage, freight.ShortDigest(o.Freight[strings.LastIndex(o.Freight, "@")+1:]), status)
		if o.Result != nil {
			fmt.Printf("   apply: %s\n", o.Result.Summary)
		}
		if o.Health != nil {
			fmt.Printf("   wait:  %s\n", o.Health.Message)
		}
	}
	return err
}

type kv map[string]string

func (m kv) String() string { return fmt.Sprint(map[string]string(m)) }
func (m kv) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if !ok {
		return fmt.Errorf("expected key=value")
	}
	m[k] = v
	return nil
}

func cmdAttest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("attest", flag.ContinueOnError)
	ref := fs.String("freight", "", "registry/repo@sha256:… (required)")
	kind := fs.String("kind", "", "evidence kind, e.g. integration-test (required)")
	stage := fs.String("stage", "", "stage the evidence was produced in")
	result := fs.String("result", "pass", "pass or fail")
	file := fs.String("file", "", "optional JSON payload (report)")
	at := fs.String("artifact-type", freight.EvidenceArtifactType, "evidence artifact type")
	extra := kv{}
	fs.Var(extra, "annotation", "extra annotation key=value (repeatable)")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *ref == "" || *kind == "" {
		fs.Usage()
		return exitError{2}
	}
	ann := map[string]string{"ship.dev/kind": *kind, "ship.dev/result": *result}
	if *stage != "" {
		ann["ship.dev/stage"] = *stage
	}
	if u := os.Getenv("CI_PIPELINE_URL"); u != "" {
		ann["ship.dev/pipeline"] = u
	}
	for k, v := range extra {
		ann[k] = v
	}
	var payload []byte
	if *file != "" {
		var err error
		if payload, err = os.ReadFile(*file); err != nil {
			return err
		}
	}
	d, err := registry(nil).PushEvidence(ctx, *ref, *at, ann, payload)
	if err != nil {
		return err
	}
	fmt.Println(d.Ref)
	return nil
}

// ---- verify-env-mr ----

func cmdVerifyEnvMR(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify-env-mr", flag.ContinueOnError)
	iidDefault, _ := strconv.Atoi(os.Getenv("CI_MERGE_REQUEST_IID"))
	iid := fs.Int("mr", iidDefault, "env MR iid (CI_MERGE_REQUEST_IID)")
	project := fs.String("project", os.Getenv("CI_PROJECT_PATH"), "env repo project path")
	ledgerProject := fs.String("ledger-project", os.Getenv("SHIP_LEDGER_PROJECT"), "ledger project path")
	noApprove := fs.Bool("no-approve", false, "check only")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *iid == 0 || *project == "" || *ledgerProject == "" {
		return fmt.Errorf("--mr, --project and --ledger-project (SHIP_LEDGER_PROJECT) are required")
	}
	gl := gitlabClient("SHIP_APPROVER_TOKEN")
	if gl == nil {
		return fmt.Errorf("SHIP_APPROVER_TOKEN and SHIP_GITLAB_URL/CI_SERVER_URL are required")
	}
	v, err := envmr.Verifier{GitLab: gl, Reg: registry(nil), LedgerProject: *ledgerProject, EnvProject: *project}.Verify(ctx, *iid, !*noApprove)
	if err != nil {
		return err
	}
	if !v.ShipMR {
		fmt.Println("not a ship MR (source branch is not ship/…): normal approval rules apply")
		return nil
	}
	for _, c := range v.Checks {
		fmt.Println("PASS", c)
	}
	for _, f := range v.Failures {
		fmt.Println("FAIL", f)
	}
	if len(v.Failures) > 0 {
		fmt.Println("provenance check failed: not approving")
		return exitError{1}
	}
	if v.Approved {
		fmt.Println("approved as ship-approver")
	}
	return nil
}
