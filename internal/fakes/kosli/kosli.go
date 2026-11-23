// Package kosli is a stateful fake of the Kosli API v2, covering what ship's
// Kosli integration needs (docs/kosli.md) and the reporting calls used to seed
// it, behind a contract.Guard built from Kosli's official OpenAPI 3.1 document
// (internal/fakes/specs).
//
// Implemented: flows, trails, artifacts, generic and custom attestations (with
// jq evaluators), custom attestation types, environments, K8S snapshot reports,
// snapshot queries (all documented snapshot expressions), environment events,
// policies (a subset, see below), attaching policies, and assert artifact.
//
// Fake limitations (fail loudly rather than guess):
//   - Policy rules with `if` or `exceptions` are rejected when the policy is created.
//   - Event types for artifacts appearing/disappearing are "started"/"exited"; the
//     spec types the field as a free string, so these names are not spec-confirmed.
//   - Only K8S environment reports are implemented.
package kosli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/itchyny/gojq"
	"gopkg.in/yaml.v3"

	"juliusbaer.com/codeak/ship/internal/fakes/contract"
	"juliusbaer.com/codeak/ship/internal/fakes/specs"
)

const htmlBase = "https://app.kosli.com"

type artifact struct {
	Fingerprint, Filename, Flow, Trail, GitCommit, CommitURL, BuildURL string
	Created                                                            float64
}

type attestation struct {
	ID, Flow, Trail, Name, Type, Fingerprint, ReportedBy, OriginURL string
	Compliant                                                       bool
	UserData, Data                                                  any
	Annotations                                                     map[string]string
	Created                                                         float64
}

type running struct {
	Fingerprint, Name string
	Pods              []int64 // creation timestamps
}

type snapshot struct {
	Index     int
	Timestamp float64
	Artifacts []running
}

type environment struct {
	Name, Type, Description string
	Policies                []string
	Snapshots               []*snapshot
}

type rule struct {
	Name           string `yaml:"name"`
	Type           string `yaml:"type"`
	MustBeComplete *bool  `yaml:"must_be_compliant"`
	If             any    `yaml:"if"`
	Exceptions     any    `yaml:"exceptions"`
}

type boolRule struct {
	Required   bool `yaml:"required"`
	Exceptions any  `yaml:"exceptions"`
}

type policyDoc struct {
	Schema    string `yaml:"_schema"`
	Artifacts struct {
		Provenance      *boolRule `yaml:"provenance"`
		TrailCompliance *boolRule `yaml:"trail-compliance"`
		Attestations    []rule    `yaml:"attestations"`
	} `yaml:"artifacts"`
}

type policy struct {
	Name, Description, CreatedBy string
	Created, Modified            float64
	Versions                     []policyDoc
}

type attType struct {
	Name, Description string
	Rules             []string
}

// Fake holds the state of one Kosli organization.
type Fake struct {
	Org string
	// Tokens maps API tokens to the identity recorded as reported_by / created_by
	// (e.g. a service account name).
	Tokens map[string]string
	// Now is the clock for timestamps (default time.Now).
	Now func() time.Time

	doc     *openapi3.T
	violate func(string)

	mu       sync.Mutex
	seq      int
	flows    map[string]bool
	trails   map[string]map[string]bool
	arts     map[string]*artifact
	atts     []*attestation
	types    map[string]*attType
	envs     map[string]*environment
	policies map[string]*policy
}

func New(org string, tokens map[string]string) *Fake {
	return &Fake{Org: org, Tokens: tokens, flows: map[string]bool{}, trails: map[string]map[string]bool{},
		arts: map[string]*artifact{}, types: map[string]*attType{}, envs: map[string]*environment{},
		policies: map[string]*policy{}}
}

var (
	docOnce sync.Once
	doc     *openapi3.T
	docErr  error
)

// Server returns the fake behind a contract guard for Kosli's official spec,
// mounted at /api/v2 (the real base path).
func (f *Fake) Server() (*contract.Guard, error) {
	docOnce.Do(func() { doc, docErr = contract.LoadOpenAPI3(specs.Kosli, "/api/v2") })
	if docErr != nil {
		return nil, docErr
	}
	f.doc = doc
	g, err := contract.New("kosli", doc, f.handler())
	if err != nil {
		return nil, err
	}
	f.violate = g.Record
	return g, nil
}

func (f *Fake) now() float64 {
	n := time.Now
	if f.Now != nil {
		n = f.Now
	}
	return float64(n().UnixMilli()) / 1000
}

func (f *Fake) handler() http.Handler {
	mux := http.NewServeMux()
	p := "/api/v2"
	mux.HandleFunc("PUT "+p+"/flows/{org}", f.createFlow)
	mux.HandleFunc("PUT "+p+"/trails/{org}/{flow}", f.createTrail)
	mux.HandleFunc("POST "+p+"/artifacts/{org}/{flow}", f.createArtifact)
	mux.HandleFunc("POST "+p+"/attestations/{org}/{flow}/trail/{trail}/generic", f.attest("generic"))
	mux.HandleFunc("POST "+p+"/attestations/{org}/{flow}/trail/{trail}/custom", f.attest("custom"))
	mux.HandleFunc("GET "+p+"/attestations/{org}/{flow}/artifact/{fp}/{name}", f.getArtifactAttestation)
	mux.HandleFunc("POST "+p+"/custom-attestation-types/{org}", f.createType)
	mux.HandleFunc("PUT "+p+"/environments/{org}", f.createEnvironment)
	mux.HandleFunc("PUT "+p+"/environments/{org}/{env}/report/K8S", f.reportK8S)
	mux.HandleFunc("POST "+p+"/environments/{org}/{env}/policies", f.attachPolicies)
	mux.HandleFunc("GET "+p+"/environments/{org}/{env}/events", f.events)
	mux.HandleFunc("GET "+p+"/snapshots/{org}/{env}/{expr}", f.getSnapshot)
	mux.HandleFunc("PUT "+p+"/policies/{org}", f.createPolicy)
	mux.HandleFunc("GET "+p+"/asserts/{org}/fingerprint/{fp}", f.assert)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		who := f.Tokens[tok]
		if !ok || who == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "Invalid or missing API token"})
			return
		}
		r.Header.Set("X-Fake-Identity", who)
		mux.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func msg(w http.ResponseWriter, code int, format string, a ...any) {
	writeJSON(w, code, map[string]any{"message": fmt.Sprintf(format, a...)})
}

func (f *Fake) orgOK(w http.ResponseWriter, r *http.Request) bool {
	if r.PathValue("org") != f.Org {
		msg(w, http.StatusNotFound, "Organization '%s' not found", r.PathValue("org"))
		return false
	}
	return true
}

// jsonPart reads a multipart field that the spec declares as a JSON string with
// a contentSchema, and validates it against that schema.
func (f *Fake) jsonPart(r *http.Request, field, schema string, out any) error {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		return err
	}
	raw := r.FormValue(field)
	if raw == "" {
		if fh := r.MultipartForm.File[field]; len(fh) > 0 {
			rd, err := fh[0].Open()
			if err != nil {
				return err
			}
			b, _ := io.ReadAll(rd)
			raw = string(b)
		}
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return fmt.Errorf("%s is not JSON: %w", field, err)
	}
	if f.doc != nil {
		if s := f.doc.Components.Schemas[schema]; s != nil && s.Value != nil {
			if err := s.Value.VisitJSON(v, openapi3.EnableJSONSchema2020(), openapi3.MultiErrors()); err != nil {
				e := fmt.Sprintf("request %s %s: %s does not match %s: %s", r.Method, r.URL.Path, field, schema,
					strings.Join(strings.Fields(err.Error()), " "))
				if f.violate != nil {
					f.violate(e)
				}
				return fmt.Errorf("%s", e)
			}
		}
	}
	return json.Unmarshal([]byte(raw), out)
}

func (f *Fake) createFlow(w http.ResponseWriter, r *http.Request) {
	if !f.orgOK(w, r) {
		return
	}
	var in struct{ Name string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		msg(w, 400, "%v", err)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	code := 201
	if f.flows[in.Name] {
		code = 200
	}
	f.flows[in.Name] = true
	writeJSON(w, code, "flow '"+in.Name+"' saved")
}

func (f *Fake) createTrail(w http.ResponseWriter, r *http.Request) {
	if !f.orgOK(w, r) {
		return
	}
	var in struct{ Name string }
	if err := f.jsonPart(r, "data_json", "CreateTrail", &in); err != nil {
		msg(w, 400, "%v", err)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	flow := r.PathValue("flow")
	if !f.flows[flow] {
		msg(w, 404, "Flow '%s' not found", flow)
		return
	}
	if f.trails[flow] == nil {
		f.trails[flow] = map[string]bool{}
	}
	f.trails[flow][in.Name] = true
	writeJSON(w, 201, "trail '"+in.Name+"' created")
}

func (f *Fake) createArtifact(w http.ResponseWriter, r *http.Request) {
	if !f.orgOK(w, r) {
		return
	}
	var in struct {
		Fingerprint string `json:"fingerprint"`
		Filename    string `json:"filename"`
		TrailName   string `json:"trail_name"`
		GitCommit   string `json:"git_commit"`
		BuildURL    string `json:"build_url"`
		CommitURL   string `json:"commit_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		msg(w, 400, "%v", err)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	flow := r.PathValue("flow")
	if !f.flows[flow] {
		msg(w, 404, "Flow '%s' not found", flow)
		return
	}
	if in.TrailName != "" {
		if f.trails[flow] == nil {
			f.trails[flow] = map[string]bool{}
		}
		f.trails[flow][in.TrailName] = true
	}
	f.arts[in.Fingerprint] = &artifact{Fingerprint: in.Fingerprint, Filename: in.Filename, Flow: flow,
		Trail: in.TrailName, GitCommit: in.GitCommit, CommitURL: in.CommitURL, BuildURL: in.BuildURL, Created: f.now()}
	writeJSON(w, 201, "artifact '"+in.Filename+"' reported")
}

func (f *Fake) attest(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !f.orgOK(w, r) {
			return
		}
		var in struct {
			Name            string            `json:"attestation_name"`
			Fingerprint     string            `json:"artifact_fingerprint"`
			Compliant       *bool             `json:"is_compliant"`
			UserData        any               `json:"user_data"`
			Annotations     map[string]string `json:"annotations"`
			OriginURL       string            `json:"origin_url"`
			TypeName        string            `json:"type_name"`
			AttestationData string            `json:"attestation_data"`
		}
		schema := map[string]string{"generic": "GenericAttestationPostInput", "custom": "CustomAttestationPostInput"}[kind]
		if err := f.jsonPart(r, "data_json", schema, &in); err != nil {
			writeJSON(w, 422, map[string]any{"message": err.Error()})
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		flow, trail := r.PathValue("flow"), r.PathValue("trail")
		if !f.trails[flow][trail] {
			msg(w, 404, "Trail '%s' not found in flow '%s'", trail, flow)
			return
		}
		a := &attestation{Flow: flow, Trail: trail, Name: in.Name, Fingerprint: in.Fingerprint,
			ReportedBy: r.Header.Get("X-Fake-Identity"), OriginURL: in.OriginURL, UserData: in.UserData,
			Annotations: in.Annotations, Created: f.now()}
		f.seq++
		a.ID = fmt.Sprintf("%08x-0000-4000-8000-%012x", f.seq, f.seq)
		switch kind {
		case "generic":
			a.Type, a.Compliant = "generic", in.Compliant != nil && *in.Compliant
		case "custom":
			t := f.types[in.TypeName]
			if t == nil {
				writeJSON(w, 422, map[string]any{"message": "custom attestation type '" + in.TypeName + "' not found"})
				return
			}
			var data any
			if err := json.Unmarshal([]byte(in.AttestationData), &data); err != nil {
				writeJSON(w, 422, map[string]any{"message": "attestation_data is not JSON: " + err.Error()})
				return
			}
			ok, err := evalJQ(t.Rules, data)
			if err != nil {
				writeJSON(w, 422, map[string]any{"message": err.Error()})
				return
			}
			a.Type, a.Data, a.Compliant = "custom:"+t.Name, data, ok
		}
		f.atts = append(f.atts, a)
		writeJSON(w, 201, "attestation '"+a.Name+"' reported")
	}
}

// evalJQ: compliant when every rule evaluates to true (Kosli's jq evaluator).
func evalJQ(rules []string, data any) (bool, error) {
	for _, src := range rules {
		q, err := gojq.Parse(src)
		if err != nil {
			return false, fmt.Errorf("jq rule %q: %w", src, err)
		}
		it := q.Run(data)
		v, ok := it.Next()
		if !ok {
			return false, nil
		}
		if err, isErr := v.(error); isErr {
			return false, fmt.Errorf("jq rule %q: %w", src, err)
		}
		if b, _ := v.(bool); !b {
			return false, nil
		}
	}
	return true, nil
}

func (f *Fake) createType(w http.ResponseWriter, r *http.Request) {
	if !f.orgOK(w, r) {
		return
	}
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Evaluator   *struct {
			ContentType string   `json:"content_type"`
			Rules       []string `json:"rules"`
		} `json:"evaluator"`
	}
	if err := f.jsonPart(r, "data_json", "CustomAttestationTypePostInput", &in); err != nil {
		msg(w, 400, "%v", err)
		return
	}
	t := &attType{Name: in.Name, Description: in.Description}
	if in.Evaluator != nil && in.Evaluator.ContentType != "default" {
		t.Rules = in.Evaluator.Rules
	}
	f.mu.Lock()
	f.types[in.Name] = t
	f.mu.Unlock()
	writeJSON(w, 201, "custom attestation type '"+in.Name+"' saved")
}

func (f *Fake) latestAttestation(fp, name string) *attestation {
	for i := len(f.atts) - 1; i >= 0; i-- {
		if a := f.atts[i]; a.Fingerprint == fp && a.Name == name {
			return a
		}
	}
	return nil
}

func (a *attestation) json() map[string]any {
	// Field names follow the example in the official AttestationResponse schema.
	m := map[string]any{"attestation_id": a.ID, "attestation_name": a.Name, "attestation_type": a.Type,
		"html_url":          fmt.Sprintf("%s/flows/%s/trails/%s?attestation_id=%s", htmlBase, a.Flow, a.Trail, a.ID),
		"has_audit_package": false, "schema_version": 1,
		"is_compliant": a.Compliant, "flow_name": a.Flow, "trail_name": a.Trail,
		"artifact_fingerprint": a.Fingerprint, "reported_by": a.ReportedBy, "created_at": a.Created}
	if a.UserData != nil {
		m["user_data"] = a.UserData
	}
	if a.Data != nil {
		m["attestation_data"] = a.Data
	}
	if a.Annotations != nil {
		m["annotations"] = a.Annotations
	}
	if a.OriginURL != "" {
		m["origin_url"] = a.OriginURL
	}
	return m
}

func (f *Fake) getArtifactAttestation(w http.ResponseWriter, r *http.Request) {
	if !f.orgOK(w, r) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// The official response is an array; newest first, so [0] is the one that counts.
	out := []map[string]any{}
	for i := len(f.atts) - 1; i >= 0; i-- {
		if a := f.atts[i]; a.Fingerprint == r.PathValue("fp") && a.Name == r.PathValue("name") && a.Flow == r.PathValue("flow") {
			out = append(out, a.json())
		}
	}
	if len(out) == 0 {
		msg(w, 404, "Attestation '%s' not found for artifact '%s'", r.PathValue("name"), r.PathValue("fp"))
		return
	}
	writeJSON(w, 200, out)
}

func (f *Fake) createEnvironment(w http.ResponseWriter, r *http.Request) {
	if !f.orgOK(w, r) {
		return
	}
	var in struct {
		Name, Type, Description string
		Policies                []string
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		msg(w, 400, "%v", err)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.envs[in.Name]
	if e == nil {
		e = &environment{Name: in.Name}
		f.envs[in.Name] = e
	}
	e.Type, e.Description = in.Type, in.Description
	if in.Policies != nil {
		e.Policies = in.Policies
	}
	writeJSON(w, 201, "environment '"+in.Name+"' saved")
}

func (f *Fake) reportK8S(w http.ResponseWriter, r *http.Request) {
	if !f.orgOK(w, r) {
		return
	}
	var in struct {
		Artifacts []struct {
			PodName           string            `json:"podName"`
			Digests           map[string]string `json:"digests"`
			CreationTimestamp int64             `json:"creationTimestamp"`
		} `json:"artifacts"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		msg(w, 400, "%v", err)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.envs[r.PathValue("env")]
	if e == nil {
		msg(w, 404, "Environment '%s' not found", r.PathValue("env"))
		return
	}
	if e.Type != "K8S" {
		msg(w, 400, "Environment '%s' has type %s, not K8S", e.Name, e.Type)
		return
	}
	byFP := map[string]*running{}
	for _, a := range in.Artifacts {
		for name, fp := range a.Digests {
			if byFP[fp] == nil {
				byFP[fp] = &running{Fingerprint: fp, Name: name}
			}
			byFP[fp].Pods = append(byFP[fp].Pods, a.CreationTimestamp)
		}
	}
	var now []running
	for _, x := range byFP {
		slices.Sort(x.Pods)
		now = append(now, *x)
	}
	sort.Slice(now, func(i, j int) bool { return now[i].Fingerprint < now[j].Fingerprint })
	// Kosli only creates a new snapshot when what is running changes.
	if n := len(e.Snapshots); n > 0 && sameSet(e.Snapshots[n-1].Artifacts, now) {
		writeJSON(w, 200, "environment unchanged")
		return
	}
	e.Snapshots = append(e.Snapshots, &snapshot{Index: len(e.Snapshots) + 1, Timestamp: f.now(), Artifacts: now})
	writeJSON(w, 201, "snapshot created")
}

func sameSet(a, b []running) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Fingerprint != b[i].Fingerprint {
			return false
		}
	}
	return true
}

func (f *Fake) attachPolicies(w http.ResponseWriter, r *http.Request) {
	if !f.orgOK(w, r) {
		return
	}
	var in struct {
		PolicyNames []string `json:"policy_names"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		msg(w, 400, "%v", err)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.envs[r.PathValue("env")]
	if e == nil {
		msg(w, 404, "Environment '%s' not found", r.PathValue("env"))
		return
	}
	for _, p := range in.PolicyNames {
		if f.policies[p] == nil {
			msg(w, 404, "Policy '%s' not found", p)
			return
		}
		if !slices.Contains(e.Policies, p) {
			e.Policies = append(e.Policies, p)
		}
	}
	writeJSON(w, 200, "policies attached")
}

func (f *Fake) createPolicy(w http.ResponseWriter, r *http.Request) {
	if !f.orgOK(w, r) {
		return
	}
	var meta struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := f.jsonPart(r, "payload", "PolicyPutInput", &meta); err != nil {
		msg(w, 400, "%v", err)
		return
	}
	var raw []byte
	if fh := r.MultipartForm.File["policy_file"]; len(fh) > 0 {
		rd, err := fh[0].Open()
		if err == nil {
			raw, _ = io.ReadAll(rd)
		}
	} else {
		raw = []byte(r.FormValue("policy_file"))
	}
	var pd policyDoc
	if err := yaml.Unmarshal(raw, &pd); err != nil || pd.Schema == "" {
		msg(w, 400, "invalid policy file (needs _schema): %v", err)
		return
	}
	for _, br := range []*boolRule{pd.Artifacts.Provenance, pd.Artifacts.TrailCompliance} {
		if br != nil && br.Exceptions != nil {
			msg(w, 400, "fake Kosli: policy exceptions are not supported")
			return
		}
	}
	for _, ru := range pd.Artifacts.Attestations {
		if ru.If != nil || ru.Exceptions != nil {
			msg(w, 400, "fake Kosli: 'if'/'exceptions' on attestation rules are not supported")
			return
		}
		if ru.Type == "" {
			msg(w, 400, "attestation rule needs a type")
			return
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.policies[meta.Name]
	code := 200
	if p == nil {
		p = &policy{Name: meta.Name, CreatedBy: r.Header.Get("X-Fake-Identity"), Created: f.now()}
		f.policies[meta.Name] = p
		code = 201
	}
	p.Description, p.Modified = meta.Description, f.now()
	p.Versions = append(p.Versions, pd) // policies are immutable: each update is a new version
	writeJSON(w, code, map[string]any{"name": p.Name, "description": p.Description, "created_by": p.CreatedBy,
		"created_at": p.Created, "last_modified_at": p.Modified, "archived": false,
		"latest_version": len(p.Versions), "versions": []any{}, "events": []any{}, "consuming_envs": f.consumers(p.Name)})
}

func (f *Fake) consumers(policy string) []string {
	out := []string{}
	for _, e := range f.envs {
		if slices.Contains(e.Policies, policy) {
			out = append(out, e.Name)
		}
	}
	sort.Strings(out)
	return out
}

// ---- compliance ----

type ruleResult struct {
	Rule       map[string]any
	Satisfied  bool
	Resolution string
}

func (f *Fake) evalPolicy(p *policy, a *artifact) (bool, []ruleResult) {
	pd := p.Versions[len(p.Versions)-1]
	var out []ruleResult
	ok := true
	add := func(r ruleResult) {
		out = append(out, r)
		ok = ok && r.Satisfied
	}
	if br := pd.Artifacts.Provenance; br != nil && br.Required {
		sat := a.Flow != ""
		add(ruleResult{Rule: map[string]any{"type": "provenance", "definition": map[string]any{"required": true}},
			Satisfied: sat, Resolution: map[bool]string{true: "rule_satisfied", false: "missing_provenance"}[sat]})
	}
	if br := pd.Artifacts.TrailCompliance; br != nil && br.Required {
		sat := f.trailCompliant(a.Flow, a.Trail)
		add(ruleResult{Rule: map[string]any{"type": "trail-compliance", "definition": map[string]any{"required": true}},
			Satisfied: sat, Resolution: map[bool]string{true: "rule_satisfied", false: "non_compliant_in_trail"}[sat]})
	}
	for _, ru := range pd.Artifacts.Attestations {
		name := ru.Name
		if name == "" {
			name = "*"
		}
		must := ru.MustBeComplete == nil || *ru.MustBeComplete
		def := map[string]any{"type": ru.Type, "name": name, "must_be_compliant": must}
		latest := map[string]*attestation{}
		for _, x := range f.atts {
			if x.Fingerprint == a.Fingerprint && (name == "*" || x.Name == name) && (ru.Type == "*" || x.Type == ru.Type) {
				latest[x.Name] = x
			}
		}
		res := ruleResult{Rule: map[string]any{"type": "attestation", "definition": def}}
		switch {
		case len(latest) == 0:
			res.Resolution = "missing_attestation"
		default:
			res.Satisfied, res.Resolution = true, "rule_satisfied"
			for _, x := range latest {
				if must && !x.Compliant {
					res.Satisfied, res.Resolution = false, "non_compliant_attestation"
				}
			}
		}
		add(res)
	}
	return ok, out
}

func (f *Fake) trailCompliant(flow, trail string) bool {
	latest := map[string]*attestation{}
	for _, x := range f.atts {
		if x.Flow == flow && x.Trail == trail {
			latest[x.Name] = x
		}
	}
	for _, x := range latest {
		if !x.Compliant {
			return false
		}
	}
	return true
}

func (f *Fake) assert(w http.ResponseWriter, r *http.Request) {
	if !f.orgOK(w, r) {
		return
	}
	q := r.URL.Query()
	if q.Get("environment_name") != "" && len(q["policy_name"]) > 0 {
		msg(w, 400, "environment_name and policy_name are mutually exclusive")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fp := r.PathValue("fp")
	a := f.arts[fp]
	if a == nil || (q.Get("flow_name") != "" && a.Flow != q.Get("flow_name")) {
		msg(w, 404, "Artifact with fingerprint '%s' not found", fp)
		return
	}
	var names []string
	scope := "flow"
	var envName any
	switch {
	case len(q["policy_name"]) > 0:
		scope = "policy"
		for _, v := range q["policy_name"] {
			names = append(names, strings.Split(v, ",")...)
		}
	case q.Get("environment_name") != "":
		scope = "environment"
		e := f.envs[q.Get("environment_name")]
		if e == nil {
			msg(w, 404, "Environment '%s' not found", q.Get("environment_name"))
			return
		}
		names, envName = e.Policies, e.Name
	}
	compliant := true
	evals := []map[string]any{}
	for _, n := range names {
		p := f.policies[n]
		if p == nil {
			msg(w, 404, "Policy '%s' not found", n)
			return
		}
		ok, rules := f.evalPolicy(p, a)
		compliant = compliant && ok
		re := []map[string]any{}
		for _, x := range rules {
			re = append(re, map[string]any{"rule": x.Rule, "satisfied": x.Satisfied, "ignored": false,
				"resolutions": []map[string]any{{"type": x.Resolution}}})
		}
		evals = append(evals, map[string]any{"policy_name": n, "policy_version": len(p.Versions),
			"status": map[bool]string{true: "COMPLIANT", false: "NON-COMPLIANT"}[ok], "rule_evaluations": re})
	}
	trailOK := f.trailCompliant(a.Flow, a.Trail)
	if scope == "flow" {
		compliant = trailOK
	}
	statuses := []map[string]any{}
	for _, x := range f.atts {
		if x.Fingerprint == fp && f.latestAttestation(fp, x.Name) == x {
			statuses = append(statuses, map[string]any{"attestation_name": x.Name, "attestation_type": x.Type,
				"is_compliant": x.Compliant, "status": map[bool]string{true: "COMPLETE", false: "INCOMPLETE"}[x.Compliant], "unexpected": false})
		}
	}
	out := map[string]any{
		"fingerprint": fp, "flow": a.Flow, "trail": a.Trail, "compliant": compliant, "scope": scope,
		"html_url": fmt.Sprintf("%s/%s/flows/%s/artifacts/%s", htmlBase, f.Org, a.Flow, fp),
		"compliance_status": map[string]any{"artifact_fingerprint": fp, "is_compliant": trailOK,
			"attestations_statuses": statuses, "evaluated_at": f.now(), "unexpected": false},
		"policy_evaluations": evals,
	}
	if envName != nil {
		out["environment"] = envName
	}
	writeJSON(w, 200, out)
}

// ---- snapshots and events ----

var (
	reAgo = regexp.MustCompile(`^@\{(\d+)\.(hours|days|weeks|months)\.ago\}$`)
	reAt  = regexp.MustCompile(`^@\{(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})\}$`)
)

// resolve implements the documented snapshot expressions.
func (f *Fake) resolve(e *environment, expr string) (*snapshot, error) {
	n := len(e.Snapshots)
	at := func(ts float64) *snapshot {
		var hit *snapshot
		for _, s := range e.Snapshots {
			if s.Timestamp <= ts {
				hit = s
			}
		}
		return hit
	}
	switch {
	case n == 0:
		return nil, fmt.Errorf("environment '%s' has no snapshots", e.Name)
	case expr == "@{now}":
		return e.Snapshots[n-1], nil
	case strings.HasPrefix(expr, "~"):
		k, err := strconv.Atoi(expr[1:])
		if err != nil || k < 0 || k >= n {
			return nil, fmt.Errorf("invalid snapshot expression %q", expr)
		}
		return e.Snapshots[n-1-k], nil
	case strings.HasPrefix(expr, "-"):
		k, err := strconv.Atoi(expr[1:])
		if err != nil || k < 1 || k > n {
			return nil, fmt.Errorf("invalid snapshot expression %q", expr)
		}
		return e.Snapshots[n-k], nil
	case reAgo.MatchString(expr):
		m := reAgo.FindStringSubmatch(expr)
		k, _ := strconv.Atoi(m[1])
		unit := map[string]float64{"hours": 3600, "days": 86400, "weeks": 604800, "months": 2592000}[m[2]]
		if s := at(f.now() - float64(k)*unit); s != nil {
			return s, nil
		}
	case reAt.MatchString(expr):
		t, err := time.Parse("2006-01-02T15:04:05", reAt.FindStringSubmatch(expr)[1])
		if err == nil {
			if s := at(float64(t.Unix())); s != nil {
				return s, nil
			}
		}
	default:
		k, err := strconv.Atoi(expr)
		if err == nil && k >= 1 && k <= n {
			return e.Snapshots[k-1], nil
		}
	}
	return nil, fmt.Errorf("no snapshot matches %q", expr)
}

func (f *Fake) flowRefs(fp string) (string, []map[string]any) {
	a := f.arts[fp]
	if a == nil {
		return "", []map[string]any{}
	}
	return a.Flow, []map[string]any{{"flow_name": a.Flow, "trail_name": a.Trail, "git_commit": a.GitCommit,
		"commit_url": a.CommitURL, "html_url": fmt.Sprintf("%s/%s/flows/%s/artifacts/%s", htmlBase, f.Org, a.Flow, fp),
		"flow_html_url": fmt.Sprintf("%s/%s/flows/%s", htmlBase, f.Org, a.Flow)}}
}

func (f *Fake) getSnapshot(w http.ResponseWriter, r *http.Request) {
	if !f.orgOK(w, r) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.envs[r.PathValue("env")]
	if e == nil {
		msg(w, 404, "Environment '%s' not found", r.PathValue("env"))
		return
	}
	s, err := f.resolve(e, r.PathValue("expr"))
	if err != nil {
		msg(w, 404, "%v", err)
		return
	}
	var prev map[string]bool
	if s.Index > 1 {
		prev = map[string]bool{}
		for _, x := range e.Snapshots[s.Index-2].Artifacts {
			prev[x.Fingerprint] = true
		}
	}
	snapOK := true
	counts := map[string]int{"compliant": 0, "non_compliant": 0}
	arts := []map[string]any{}
	for _, x := range s.Artifacts {
		flow, flows := f.flowRefs(x.Fingerprint)
		ok := true
		var decisions []map[string]any
		if a := f.arts[x.Fingerprint]; a != nil {
			for _, pn := range e.Policies {
				if p := f.policies[pn]; p != nil {
					c, _ := f.evalPolicy(p, a)
					ok = ok && c
					decisions = append(decisions, map[string]any{"policy_name": pn, "policy_version": len(p.Versions),
						"status": map[bool]string{true: "COMPLIANT", false: "NON-COMPLIANT"}[c], "rule_evaluations": []any{}})
				}
			}
		} else {
			ok = len(e.Policies) == 0 // unknown artifact: no provenance
		}
		snapOK = snapOK && ok
		counts[map[bool]string{true: "compliant", false: "non_compliant"}[ok]]++
		ann := map[string]any{"type": "unchanged"}
		if prev == nil || !prev[x.Fingerprint] {
			ann = map[string]any{"type": "started"}
		}
		item := map[string]any{"name": x.Name, "fingerprint": x.Fingerprint, "creationTimestamp": x.Pods,
			"annotation": ann, "flow_name": flow, "git_commit": "", "commit_url": "", "compliant": ok,
			"html_url":      fmt.Sprintf("%s/%s/environments/%s/snapshots/%d", htmlBase, f.Org, e.Name, s.Index),
			"flow_html_url": "", "flows": flows, "deployments": []int{}}
		if a := f.arts[x.Fingerprint]; a != nil {
			item["git_commit"], item["commit_url"] = a.GitCommit, a.CommitURL
			item["flow_html_url"] = fmt.Sprintf("%s/%s/flows/%s", htmlBase, f.Org, a.Flow)
		}
		if decisions != nil {
			item["policy_decisions"] = decisions
		}
		arts = append(arts, item)
	}
	applied := []map[string]any{}
	for _, pn := range e.Policies {
		applied = append(applied, map[string]any{"name": pn})
	}
	out := map[string]any{"index": s.Index, "is_latest": s.Index == len(e.Snapshots),
		"artifact_compliance_count": counts, "timestamp": s.Timestamp, "type": e.Type, "compliant": snapOK,
		"html_url":  fmt.Sprintf("%s/%s/environments/%s/snapshots/%d", htmlBase, f.Org, e.Name, s.Index),
		"artifacts": arts, "applied_policies": applied}
	if s.Index < len(e.Snapshots) {
		out["next_snapshot_timestamp"] = e.Snapshots[s.Index].Timestamp
	}
	writeJSON(w, 200, out)
}

// events derives "started"/"exited" events from consecutive snapshots,
// newest first unless reverse=true.
func (f *Fake) events(w http.ResponseWriter, r *http.Request) {
	if !f.orgOK(w, r) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.envs[r.PathValue("env")]
	if e == nil {
		msg(w, 404, "Environment '%s' not found", r.PathValue("env"))
		return
	}
	ev := func(typ string, s *snapshot, x running) map[string]any {
		_, flows := f.flowRefs(x.Fingerprint)
		fl := []map[string]any{}
		for _, fr := range flows {
			fl = append(fl, map[string]any{"flow_name": fr["flow_name"]})
		}
		return map[string]any{"type": typ, "environment_name": e.Name, "snapshot_index": s.Index,
			"artifact_name": x.Name, "sha256": x.Fingerprint, "reported_at": s.Timestamp, "flows": fl,
			"description": fmt.Sprintf("%s %s", x.Name, typ)}
	}
	out := []map[string]any{}
	var prev []running
	for _, s := range e.Snapshots {
		was, now := map[string]running{}, map[string]bool{}
		for _, x := range prev {
			was[x.Fingerprint] = x
		}
		for _, x := range s.Artifacts {
			now[x.Fingerprint] = true
			if _, ok := was[x.Fingerprint]; !ok {
				out = append(out, ev("started", s, x))
			}
		}
		for _, x := range prev {
			if !now[x.Fingerprint] {
				out = append(out, ev("exited", s, x))
			}
		}
		prev = s.Artifacts
	}
	if r.URL.Query().Get("reverse") != "true" {
		slices.Reverse(out)
	}
	w.Header().Set("X-Total-Count", strconv.Itoa(len(out)))
	writeJSON(w, 200, out)
}
