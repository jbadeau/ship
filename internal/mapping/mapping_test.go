package mapping

import (
	"strings"
	"testing"
)

const claim = `# owned by the SV-API_GW_MGT team
apiVersion: example.org/v1alpha1
kind: AppDeployment
metadata:
  name: gw
spec:
  replicas: 3 # tuned by hand
  artifact:
    ref: old
`

func TestRender(t *testing.T) {
	m, err := Parse([]byte(`
apiVersion: example.org/v1alpha1
kind: AppDeployment
set:
  - path: spec.artifact.ref
    value: "{{ .Freight.Ref }}"
  - path: metadata.annotations["ship.dev/freight"]
    value: "{{ .Freight.Digest }}"
  - path: spec.version
    value: '{{ index .Freight.Annotations "org.opencontainers.image.version" }}'
  - path: spec.enabled
    value: "true"
`))
	if err != nil {
		t.Fatal(err)
	}
	var d Data
	d.Freight.Ref, d.Freight.Digest = "reg/gw@sha256:abc", "sha256:abc"
	d.Freight.Annotations = map[string]string{"org.opencontainers.image.version": "1.0.0"}
	out, err := Render([]byte(claim), m, d)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"# owned by the SV-API_GW_MGT team", "replicas: 3 # tuned by hand",
		"ref: reg/gw@sha256:abc", `ship.dev/freight: sha256:abc`, `version: 1.0.0`, `enabled: "true"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	// Idempotent: rendering the output again yields the same bytes.
	again, _ := Render(out, m, d)
	if string(again) != s {
		t.Errorf("not idempotent:\n%s\nvs\n%s", s, again)
	}
}

func TestRenderKindMismatch(t *testing.T) {
	m, _ := Parse([]byte("kind: Other\nset: [{path: a, value: b}]\n"))
	if _, err := Render([]byte(claim), m, Data{}); err == nil {
		t.Fatal("expected kind mismatch")
	}
}

func TestSplitPath(t *testing.T) {
	got, err := splitPath(`metadata.annotations["a.b/c"].x`)
	if err != nil || strings.Join(got, "|") != "metadata|annotations|a.b/c|x" {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range []string{"", "a..b", `a["x`, ".a", "a.", `a[""]`} {
		if _, err := splitPath(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}
