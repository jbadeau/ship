// Package driver defines the deployment driver contract (docs/design.md §7).
package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/jbadeau/ship/internal/freight"
	"github.com/jbadeau/ship/internal/gitlab"
)

// ErrNotSupported is returned by Wait when the driver cannot observe health.
var ErrNotSupported = errors.New("not supported by driver")

type Driver interface {
	// Plan describes what Apply would change. No side effects.
	Plan(ctx context.Context, req Request) (Plan, error)
	// Apply makes the target reflect req.Freight. Must be idempotent.
	Apply(ctx context.Context, req Request) (Result, error)
	// Wait blocks until the target is healthy at req.Freight or ctx expires.
	Wait(ctx context.Context, req Request) (Health, error)
}

type Request struct {
	App       string              `json:"app"`
	Stage     string              `json:"stage"`
	Freight   freight.Descriptor  `json:"freight"`
	Previous  *freight.Descriptor `json:"previous,omitempty"`
	Config    map[string]any      `json:"config"`
	Promotion Promotion           `json:"promotion"`
	// Files reads files from the ledger repo at the promotion commit (e.g. mapping files).
	Files Files `json:"-"`
}

type Promotion struct {
	LedgerProject string `json:"ledgerProject,omitempty"`
	LedgerCommit  string `json:"ledgerCommit,omitempty"`
	LedgerMR      int    `json:"ledgerMR,omitempty"`
	LedgerMRURL   string `json:"ledgerMRURL,omitempty"`
	PipelineURL   string `json:"pipelineURL,omitempty"`
}

type Files interface {
	ReadFile(path string) ([]byte, error)
}

type Plan struct {
	Summary string `json:"summary"`
	Diff    string `json:"diff,omitempty"`
	NoOp    bool   `json:"noop,omitempty"`
}

type Result struct {
	Changed bool     `json:"changed"`
	Summary string   `json:"summary"`
	Links   []string `json:"links,omitempty"`
}

type Health struct {
	Ready   bool   `json:"ready"`
	Message string `json:"message"`
}

// Deps are shared clients handed to drivers.
type Deps struct {
	GitLab   *gitlab.Client
	Registry freight.Registry
	Log      io.Writer
}

func (d Deps) Logf(format string, a ...any) {
	if d.Log != nil {
		fmt.Fprintf(d.Log, format+"\n", a...)
	}
}

// ---- noop ----

type Noop struct{ Deps Deps }

func (n Noop) Plan(_ context.Context, r Request) (Plan, error) {
	return Plan{Summary: fmt.Sprintf("noop: would record %s/%s -> %s", r.App, r.Stage, r.Freight.Ref)}, nil
}
func (n Noop) Apply(_ context.Context, r Request) (Result, error) {
	n.Deps.Logf("noop: %s/%s -> %s", r.App, r.Stage, r.Freight.Ref)
	return Result{Summary: "noop"}, nil
}
func (n Noop) Wait(context.Context, Request) (Health, error) { return Health{}, ErrNotSupported }

// ---- exec: run a configured shell command for apply ----

type Exec struct{ Deps Deps }

func (e Exec) Plan(_ context.Context, r Request) (Plan, error) {
	cmd, _ := r.Config["command"].(string)
	return Plan{Summary: fmt.Sprintf("exec: would run %q with SHIP_FREIGHT=%s", cmd, r.Freight.Ref)}, nil
}

func (e Exec) Apply(ctx context.Context, r Request) (Result, error) {
	cmd, _ := r.Config["command"].(string)
	if cmd == "" {
		return Result{}, fmt.Errorf("exec driver: config.command is required")
	}
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Env = append(os.Environ(),
		"SHIP_APP="+r.App, "SHIP_STAGE="+r.Stage,
		"SHIP_FREIGHT="+r.Freight.Ref, "SHIP_FREIGHT_DIGEST="+r.Freight.Digest,
		"SHIP_FREIGHT_REPOSITORY="+r.Freight.Repository, "SHIP_FREIGHT_ARTIFACT_TYPE="+r.Freight.ArtifactType,
		"SHIP_LEDGER_COMMIT="+r.Promotion.LedgerCommit, fmt.Sprintf("SHIP_LEDGER_MR=%d", r.Promotion.LedgerMR))
	c.Stdout, c.Stderr = e.Deps.Log, e.Deps.Log
	if err := c.Run(); err != nil {
		return Result{}, fmt.Errorf("exec driver: %w", err)
	}
	return Result{Changed: true, Summary: "ran " + cmd}, nil
}

func (e Exec) Wait(context.Context, Request) (Health, error) { return Health{}, ErrNotSupported }

// ---- external: ship-driver-<name> speaking JSON over stdin/stdout ----

// External invokes `<Path> plan|apply|wait` with the request as JSON on stdin and
// expects {"plan"|"result"|"health": …} or {"error": "…"} / {"notSupported": true} on stdout.
type External struct {
	Path string
	Deps Deps
}

type externalResponse struct {
	Plan         *Plan   `json:"plan,omitempty"`
	Result       *Result `json:"result,omitempty"`
	Health       *Health `json:"health,omitempty"`
	NotSupported bool    `json:"notSupported,omitempty"`
	Error        string  `json:"error,omitempty"`
}

func (x External) call(ctx context.Context, op string, r Request) (externalResponse, error) {
	in, err := json.Marshal(r)
	if err != nil {
		return externalResponse{}, err
	}
	c := exec.CommandContext(ctx, x.Path, op)
	c.Stdin = bytes.NewReader(in)
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, x.Deps.Log
	if err := c.Run(); err != nil {
		return externalResponse{}, fmt.Errorf("driver %s %s: %w", x.Path, op, err)
	}
	var resp externalResponse
	dec := json.NewDecoder(&out)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&resp); err != nil {
		return resp, fmt.Errorf("driver %s %s: invalid response: %w", x.Path, op, err)
	}
	if resp.Error != "" {
		return resp, fmt.Errorf("driver %s %s: %s", x.Path, op, resp.Error)
	}
	return resp, nil
}

func (x External) Plan(ctx context.Context, r Request) (Plan, error) {
	resp, err := x.call(ctx, "plan", r)
	if err != nil {
		return Plan{}, err
	}
	if resp.Plan == nil {
		return Plan{}, fmt.Errorf("driver %s plan: missing plan", x.Path)
	}
	return *resp.Plan, nil
}

func (x External) Apply(ctx context.Context, r Request) (Result, error) {
	resp, err := x.call(ctx, "apply", r)
	if err != nil {
		return Result{}, err
	}
	if resp.Result == nil {
		return Result{}, fmt.Errorf("driver %s apply: missing result", x.Path)
	}
	return *resp.Result, nil
}

func (x External) Wait(ctx context.Context, r Request) (Health, error) {
	resp, err := x.call(ctx, "wait", r)
	if err != nil {
		return Health{}, err
	}
	if resp.NotSupported {
		return Health{}, ErrNotSupported
	}
	if resp.Health == nil {
		return Health{}, fmt.Errorf("driver %s wait: missing health", x.Path)
	}
	return *resp.Health, nil
}

// LookupExternal finds ship-driver-<name> on PATH.
func LookupExternal(name string, deps Deps) (Driver, error) {
	if strings.ContainsAny(name, "/\\ ") {
		return nil, fmt.Errorf("invalid driver name %q", name)
	}
	p, err := exec.LookPath("ship-driver-" + name)
	if err != nil {
		return nil, fmt.Errorf("unknown driver %q (not built in, and ship-driver-%s not on PATH)", name, name)
	}
	return External{Path: p, Deps: deps}, nil
}
