// Package contract validates HTTP traffic against an official OpenAPI/Swagger
// specification. Fakes of external services are wrapped in a Guard so that both
// sides are checked: requests our code sends (a wrong request is answered with
// 400, like the real service would) and responses the fake returns (a fake that
// drifts from the real API is a bug in the fake). Every mismatch is recorded as a
// violation; tests assert there are none, and the e2e sandbox exposes them at
// GET /_contract/violations.
package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	"github.com/getkin/kin-openapi/openapi2"
	"github.com/getkin/kin-openapi/openapi2conv"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
)

// LoadOpenAPI3 parses an OpenAPI 3.x document and mounts it at basePath
// (e.g. "/api/v2"), replacing the vendor's absolute server URLs.
func LoadOpenAPI3(spec []byte, basePath string) (*openapi3.T, error) {
	doc, err := openapi3.NewLoader().LoadFromData(spec)
	if err != nil {
		return nil, err
	}
	doc.Servers = openapi3.Servers{{URL: basePath}}
	return doc, nil
}

// LoadSwagger2 parses a Swagger 2.0 document, converts it to OpenAPI 3 and
// mounts it at basePath (e.g. "/api").
func LoadSwagger2(spec []byte, basePath string) (*openapi3.T, error) {
	var d2 openapi2.T
	if err := json.Unmarshal(spec, &d2); err != nil {
		return nil, err
	}
	doc, err := openapi2conv.ToV3(&d2)
	if err != nil {
		return nil, err
	}
	doc.Servers = openapi3.Servers{{URL: basePath}}
	return doc, nil
}

type Guard struct {
	Name string
	Doc  *openapi3.T
	next http.Handler

	router routers.Router
	opts   *openapi3filter.Options

	mu         sync.Mutex
	violations []string
	calls      int
}

// New wraps next. Authentication is left to the fake (it knows its tokens).
func New(name string, doc *openapi3.T, next http.Handler) (*Guard, error) {
	r, err := gorillamux.NewRouter(doc)
	if err != nil {
		return nil, fmt.Errorf("%s: router: %w", name, err)
	}
	return &Guard{Name: name, Doc: doc, next: next, router: r, opts: &openapi3filter.Options{
		AuthenticationFunc:    openapi3filter.NoopAuthenticationFunc,
		IncludeResponseStatus: true,
		MultiError:            true,
	}}, nil
}

// Record adds a violation found by the fake itself, e.g. a JSON field inside a
// multipart part that the spec describes with contentSchema (not validated by
// the generic request validator).
func (g *Guard) Record(v string) { g.record("%s", v) }

func (g *Guard) record(format string, a ...any) string {
	v := fmt.Sprintf(format, a...)
	g.mu.Lock()
	g.violations = append(g.violations, v)
	g.mu.Unlock()
	return v
}

// Violations returns every recorded mismatch so far.
func (g *Guard) Violations() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.violations...)
}

// Reset clears recorded violations (for tests that provoke them on purpose).
func (g *Guard) Reset() {
	g.mu.Lock()
	g.violations = nil
	g.mu.Unlock()
}

// Calls returns the number of validated calls.
func (g *Guard) Calls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// T is the subset of testing.TB the guard needs.
type T interface {
	Helper()
	Errorf(format string, args ...any)
}

// AssertClean fails the test for every recorded violation.
func (g *Guard) AssertClean(t T) {
	t.Helper()
	for _, v := range g.Violations() {
		t.Errorf("%s contract violation: %s", g.Name, v)
	}
}

func (g *Guard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/_contract/violations" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"spec": g.Name, "calls": g.Calls(), "violations": g.Violations()})
		return
	}
	ctx := context.Background()
	route, params, err := g.router.FindRoute(r)
	if err != nil {
		msg := g.record("%s %s: not in the official spec", r.Method, r.URL.Path)
		http.Error(w, msg, http.StatusNotFound)
		return
	}
	g.mu.Lock()
	g.calls++
	g.mu.Unlock()
	in := &openapi3filter.RequestValidationInput{Request: r, PathParams: params, Route: route, Options: g.opts}
	if err := openapi3filter.ValidateRequest(ctx, in); err != nil {
		msg := g.record("request %s %s (%s): %s", r.Method, r.URL.Path, route.Operation.OperationID, oneLine(err))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": msg})
		return
	}
	rec := httptest.NewRecorder()
	g.next.ServeHTTP(rec, r)
	body := rec.Body.Bytes()
	out := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: in, Status: rec.Code, Header: rec.Header(),
		Body: io.NopCloser(bytes.NewReader(body)), Options: g.opts,
	}
	// Neither vendor spec documents 401 (authentication happens before the API
	// layer), so an auth failure is not checked against the spec.
	if rec.Code == http.StatusUnauthorized {
	} else if err := openapi3filter.ValidateResponse(ctx, out); err != nil {
		g.record("response %d to %s %s (%s): %s", rec.Code, r.Method, r.URL.Path, route.Operation.OperationID, oneLine(err))
	}
	for k, vs := range rec.Header() {
		w.Header()[k] = vs
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(body)
}

func oneLine(err error) string {
	s := strings.Join(strings.Fields(err.Error()), " ")
	if len(s) > 600 {
		s = s[:600] + "…"
	}
	return s
}
