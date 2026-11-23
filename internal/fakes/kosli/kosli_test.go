package kosli

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"juliusbaer.com/codeak/ship/internal/fakes/contract"
)

type client struct {
	t     *testing.T
	base  string
	token string
}

type result struct {
	t    *testing.T
	code int
	body []byte
}

func (r result) want(codes ...int) []byte {
	r.t.Helper()
	for _, c := range codes {
		if r.code == c {
			return r.body
		}
	}
	r.t.Fatalf("status %d, want %v: %s", r.code, codes, r.body)
	return nil
}

func (c client) do(method, path string, body io.Reader, ctype string) result {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.base+"/api/v2"+path, body)
	req.Header.Set("Authorization", "Bearer "+c.token)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return result{c.t, resp.StatusCode, b}
}

func (c client) json(method, path string, v any) result {
	c.t.Helper()
	b, _ := json.Marshal(v)
	return c.do(method, path, bytes.NewReader(b), "application/json")
}

// multipart sends JSON-valued form fields (data_json, payload) and optional files.
func (c client) multipart(method, path string, fields map[string]any, files map[string]string) result {
	c.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		b, _ := json.Marshal(v)
		_ = mw.WriteField(k, string(b))
	}
	for k, content := range files {
		fw, _ := mw.CreateFormFile(k, k+".yml")
		_, _ = fw.Write([]byte(content))
	}
	_ = mw.Close()
	return c.do(method, path, &buf, mw.FormDataContentType())
}

func setup(t *testing.T) (*Fake, *contract.Guard, client, client, client, *time.Time) {
	now := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	f := New("bank", map[string]string{"ci-token": "ci-build", "snap-token": "k8s-snapshotter", "ship-token": "ship-deploy"})
	f.Now = func() time.Time { return now }
	g, err := f.Server()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { g.AssertClean(t) })
	mk := func(tok string) client { return client{t: t, base: srv.URL, token: tok} }
	return f, g, mk("ci-token"), mk("snap-token"), mk("ship-token"), &now
}

const fpA = "1111111111111111111111111111111111111111111111111111111111111111"
const gitSHA = "abcdefabcdefabcdefabcdefabcdefabcdefabcd"
const fpB = "2222222222222222222222222222222222222222222222222222222222222222"

func TestBuildReportsAndAssert(t *testing.T) {
	_, g, ci, _, ship, _ := setup(t)

	// Build pipeline: flow, trail, artifact, attestations.
	ci.json("PUT", "/flows/bank", map[string]any{"name": "gw", "description": "gateway build", "template": []string{}}).want(201)
	ci.multipart("PUT", "/trails/bank/gw", map[string]any{"data_json": map[string]any{"name": "build-42"}}, nil).want(201)
	ci.json("POST", "/artifacts/bank/gw", map[string]any{"fingerprint": fpA, "filename": "reg/gw:1.4.2",
		"trail_name": "build-42", "build_url": "https://ci/42", "commit_url": "https://git/c/abc", "git_commit": gitSHA}).want(201)
	ci.multipart("POST", "/attestations/bank/gw/trail/build-42/generic", map[string]any{"data_json": map[string]any{
		"attestation_name": "unit-tests", "is_compliant": true, "artifact_fingerprint": fpA}}, nil).want(201)

	// Policy requiring provenance + a compliant integration-test attestation.
	ship.multipart("PUT", "/policies/bank", map[string]any{"payload": map[string]any{"name": "prod-release", "description": "PROD gate"}},
		map[string]string{"policy_file": `_schema: https://docs.kosli.com/schemas/policy/v1
artifacts:
  provenance: {required: true}
  attestations:
    - name: integration-test
      type: generic
`}).want(201)

	assert := func() map[string]any {
		var out map[string]any
		_ = json.Unmarshal(ship.do("GET", "/asserts/bank/fingerprint/"+fpA+"?policy_name=prod-release", nil, "").want(200), &out)
		return out
	}
	if a := assert(); a["compliant"] != false || !strings.Contains(mustJSON(a), "missing_attestation") {
		t.Fatalf("expected missing attestation, got %s", mustJSON(a))
	}
	ci.multipart("POST", "/attestations/bank/gw/trail/build-42/generic", map[string]any{"data_json": map[string]any{
		"attestation_name": "integration-test", "is_compliant": false, "artifact_fingerprint": fpA}}, nil).want(201)
	if a := assert(); a["compliant"] != false || !strings.Contains(mustJSON(a), "non_compliant_attestation") {
		t.Fatalf("expected non-compliant, got %s", mustJSON(a))
	}
	ci.multipart("POST", "/attestations/bank/gw/trail/build-42/generic", map[string]any{"data_json": map[string]any{
		"attestation_name": "integration-test", "is_compliant": true, "artifact_fingerprint": fpA}}, nil).want(201)
	if a := assert(); a["compliant"] != true || a["scope"] != "policy" {
		t.Fatalf("expected compliant (latest attestation counts), got %s", mustJSON(a))
	}

	// reported_by is the identity behind the token (trust check, docs/kosli.md §4).
	var atts []map[string]any
	_ = json.Unmarshal(ship.do("GET", "/attestations/bank/gw/artifact/"+fpA+"/integration-test", nil, "").want(200), &atts)
	if len(atts) != 2 || atts[0]["reported_by"] != "ci-build" || atts[0]["is_compliant"] != true || atts[1]["is_compliant"] != false {
		t.Fatalf("attestations (newest first): %v", atts)
	}
	// Unknown artifact → 404.
	ship.do("GET", "/asserts/bank/fingerprint/"+fpB, nil, "").want(404)
	if g.Calls() < 10 {
		t.Fatalf("expected the guard to see all calls, saw %d", g.Calls())
	}
}

func TestCustomAttestationJQ(t *testing.T) {
	_, _, ci, _, ship, _ := setup(t)
	ci.json("PUT", "/flows/bank", map[string]any{"name": "ship-promotions", "description": "x", "template": []string{}}).want(201)
	ci.multipart("PUT", "/trails/bank/ship-promotions", map[string]any{"data_json": map[string]any{"name": "ledger-mr-7"}}, nil).want(201)
	ci.json("POST", "/artifacts/bank/ship-promotions", map[string]any{"fingerprint": fpA, "filename": "gw",
		"trail_name": "ledger-mr-7", "build_url": "u", "commit_url": "u", "git_commit": gitSHA}).want(201)
	ship.multipart("POST", "/custom-attestation-types/bank", map[string]any{"data_json": map[string]any{
		"name": "ship-promotion", "evaluator": map[string]any{"content_type": "jq",
			"rules": []string{".independent_approvals >= .required_approvals"}}}}, nil).want(201)
	promo := func(n int) {
		data, _ := json.Marshal(map[string]any{"independent_approvals": n, "required_approvals": 1, "ledger_mr": "!7"})
		ship.multipart("POST", "/attestations/bank/ship-promotions/trail/ledger-mr-7/custom", map[string]any{"data_json": map[string]any{
			"attestation_name": "promotion", "type_name": "ship-promotion", "attestation_data": string(data),
			"artifact_fingerprint": fpA}}, nil).want(201)
	}
	get := func() map[string]any {
		var a []map[string]any
		_ = json.Unmarshal(ship.do("GET", "/attestations/bank/ship-promotions/artifact/"+fpA+"/promotion", nil, "").want(200), &a)
		return a[0]
	}
	promo(0)
	if a := get(); a["is_compliant"] != false || a["attestation_type"] != "custom:ship-promotion" {
		t.Fatalf("self-approved promotion should be non-compliant: %v", a)
	}
	promo(1)
	if a := get(); a["is_compliant"] != true || a["reported_by"] != "ship-deploy" {
		t.Fatalf("approved promotion should be compliant: %v", a)
	}
}

func TestSnapshotsAndEvents(t *testing.T) {
	_, _, ci, snap, ship, now := setup(t)
	ci.json("PUT", "/flows/bank", map[string]any{"name": "gw", "description": "x", "template": []string{}}).want(201)
	ci.json("POST", "/artifacts/bank/gw", map[string]any{"fingerprint": fpA, "filename": "reg/gw:1.4.2",
		"build_url": "u", "commit_url": "u", "git_commit": gitSHA}).want(201)
	snap.json("PUT", "/environments/bank", map[string]any{"name": "test-k8s", "type": "K8S", "description": "TEST cluster"}).want(201)

	report := func(fps ...string) int {
		var arts []map[string]any
		for i, fp := range fps {
			arts = append(arts, map[string]any{"podName": "gw-" + fp[:4], "namespace": "gw",
				"digests": map[string]string{"reg/gw": fp}, "creationTimestamp": now.Unix() - int64(i)})
		}
		if arts == nil {
			arts = []map[string]any{}
		}
		return snap.json("PUT", "/environments/bank/test-k8s/report/K8S", map[string]any{"type": "K8S", "artifacts": arts}).code
	}
	if c := report(fpA); c != 201 {
		t.Fatalf("first report: %d", c)
	}
	*now = now.Add(time.Hour)
	if c := report(fpA); c != 200 {
		t.Fatalf("unchanged report must not create a snapshot: %d", c)
	}
	*now = now.Add(23 * time.Hour)
	if c := report(fpB); c != 201 {
		t.Fatalf("change: %d", c)
	}

	var s map[string]any
	_ = json.Unmarshal(ship.do("GET", "/snapshots/bank/test-k8s/-1", nil, "").want(200), &s)
	if s["index"] != float64(2) || s["is_latest"] != true || !strings.Contains(mustJSON(s), fpB) {
		t.Fatalf("latest snapshot: %s", mustJSON(s))
	}
	_ = json.Unmarshal(ship.do("GET", "/snapshots/bank/test-k8s/~1", nil, "").want(200), &s)
	if s["index"] != float64(1) || !strings.Contains(mustJSON(s), fpA) || s["next_snapshot_timestamp"] == nil {
		t.Fatalf("previous snapshot: %s", mustJSON(s))
	}
	_ = json.Unmarshal(ship.do("GET", "/snapshots/bank/test-k8s/@{2026-11-20T09:00:00}", nil, "").want(200), &s)
	if s["index"] != float64(1) {
		t.Fatalf("snapshot at time: %s", mustJSON(s))
	}

	// Events give "running since" and "replaced at": A ran from T0 to T0+24h.
	var evs []map[string]any
	_ = json.Unmarshal(ship.do("GET", "/environments/bank/test-k8s/events?reverse=true", nil, "").want(200), &evs)
	if len(evs) != 3 || evs[0]["type"] != "started" || evs[0]["sha256"] != fpA ||
		evs[1]["type"] != "started" || evs[1]["sha256"] != fpB || evs[2]["type"] != "exited" || evs[2]["sha256"] != fpA {
		t.Fatalf("events: %s", mustJSON(evs))
	}
	if soak := evs[2]["reported_at"].(float64) - evs[0]["reported_at"].(float64); soak != 24*3600 {
		t.Fatalf("A ran for %vs, want 24h", soak)
	}
}

func TestContractRejectsBadRequests(t *testing.T) {
	_, g, ci, _, _, _ := setup(t)
	// Missing required "template" in the flow body: rejected by the official schema.
	if code := ci.json("PUT", "/flows/bank", map[string]any{"name": "gw", "description": "x"}).code; code != 400 {
		t.Fatalf("expected 400 from the spec guard, got %d", code)
	}
	// Undocumented endpoint.
	if code := ci.do("GET", "/no/such/endpoint", nil, "").code; code != 404 {
		t.Fatalf("expected 404, got %d", code)
	}
	if n := len(g.Violations()); n != 2 {
		t.Fatalf("expected 2 recorded violations, got %v", g.Violations())
	}
	g.Reset() // these were deliberate; AssertClean runs at cleanup
}

func TestAuth(t *testing.T) {
	_, _, ci, _, _, _ := setup(t)
	bad := client{t: t, base: ci.base, token: "nope"}
	if code := bad.do("GET", "/asserts/bank/fingerprint/"+fpA, nil, "").code; code != 401 {
		t.Fatalf("expected 401, got %d", code)
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
