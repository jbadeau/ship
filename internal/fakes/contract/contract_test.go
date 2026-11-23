package contract

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const spec = `{"openapi":"3.1.0","info":{"title":"t","version":"1"},"paths":{"/things/{id}":{"get":{
  "parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"string"}}],
  "responses":{"200":{"description":"ok","content":{"application/json":{"schema":{
    "type":"object","required":["id","n"],"properties":{"id":{"type":"string"},"n":{"type":"integer"}}}}}}}}}}}`

func serve(t *testing.T, body string, code int) (*Guard, *httptest.Server) {
	doc, err := LoadOpenAPI3([]byte(spec), "/api")
	if err != nil {
		t.Fatal(err)
	}
	g, err := New("t", doc, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		io.WriteString(w, body)
	}))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	return g, srv
}

func get(t *testing.T, url string) int {
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestValidResponse(t *testing.T) {
	g, srv := serve(t, `{"id":"a","n":1}`, 200)
	if c := get(t, srv.URL+"/api/things/a"); c != 200 || len(g.Violations()) != 0 || g.Calls() != 1 {
		t.Fatalf("code %d violations %v", c, g.Violations())
	}
}

func TestDriftingFakeIsCaught(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		code int
	}{
		"missing required field": {`{"id":"a"}`, 200},
		"wrong type":             {`{"id":"a","n":"one"}`, 200},
		"undocumented status":    {`{}`, 418},
	} {
		t.Run(name, func(t *testing.T) {
			g, srv := serve(t, tc.body, tc.code)
			get(t, srv.URL+"/api/things/a")
			if v := g.Violations(); len(v) != 1 || !strings.Contains(v[0], "response") {
				t.Fatalf("expected one response violation, got %v", v)
			}
		})
	}
}

func TestUndocumentedPath(t *testing.T) {
	g, srv := serve(t, `{}`, 200)
	if c := get(t, srv.URL+"/api/other"); c != 404 || len(g.Violations()) != 1 {
		t.Fatalf("code %d violations %v", c, g.Violations())
	}
}
