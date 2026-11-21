package config

import (
	"strings"
	"testing"
)

const valid = `apiVersion: ship/v1alpha1
apps:
  SV-API_GW_MGT:
    repository: reg.example.com/deploy/gw
    deploy:
      driver: crossplane
      config: { envRepo: "sda/services/${app}/${stage}", path: claim.yaml, mapping: m.yaml }
      stages:
        PROD: { path: prod-claim.yaml }
stages:
  - name: DEV
  - name: PROD
    upstream: DEV
`

func TestDriverConfig(t *testing.T) {
	f, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	c := f.DriverConfig("SV-API_GW_MGT", "PROD")
	if c["envRepo"] != "sda/services/SV-API_GW_MGT/PROD" || c["path"] != "prod-claim.yaml" || c["mapping"] != "m.yaml" {
		t.Fatalf("got %v", c)
	}
	if f.LedgerDir != "ledger" || f.GitLab.LedgerMinApprovals != 1 {
		t.Fatalf("defaults not applied: %+v", f)
	}
}

func TestInvalid(t *testing.T) {
	for name, y := range map[string]string{
		"unknown field":     valid + "extra: 1\n",
		"upstream order":    "apiVersion: ship/v1alpha1\nstages: [{name: B, upstream: A}, {name: A}]\n",
		"tagged repo":       "apiVersion: ship/v1alpha1\nstages: [{name: A}]\napps: {x: {repository: 'r/x:1', deploy: {driver: noop}}}\n",
		"no driver":         "apiVersion: ship/v1alpha1\nstages: [{name: A}]\napps: {x: {repository: r/x}}\n",
		"unknown stage cfg": "apiVersion: ship/v1alpha1\nstages: [{name: A}]\napps: {x: {repository: r/x, deploy: {driver: noop, stages: {B: {}}}}}\n",
		"bad name":          "apiVersion: ship/v1alpha1\nstages: [{name: 'a/b'}]\n",
	} {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

const templated = `apiVersion: ship/v1alpha1
deployTemplates:
  service:
    driver: crossplane
    config: { envRepo: "sda/services/${app}/${stage}", path: claim.yaml, mapping: mappings/service.yaml }
  application:
    driver: crossplane
    config: { envRepo: "sda/applications/${group}/${stage}", path: "apps/${app}/claim.yaml", mapping: mappings/application.yaml }
    stages:
      PROD: { targetBranch: release }
apps:
  SV-API_GW_MGT:
    repository: reg/gw
    deploy: { template: service }
  portal:
    repository: reg/portal
    vars: { group: CUSTOMER_PORTAL }
    deploy:
      template: application
      config: { mapping: mappings/portal.yaml }
  legacy:
    repository: reg/legacy
    deploy:
      template: service
      driver: octopus
      config: { project: legacy, packageStep: deploy }
      stages: { DEV: { environment: Development }, PROD: { environment: Production } }
stages:
  - name: DEV
  - name: PROD
    upstream: DEV
`

func TestTemplates(t *testing.T) {
	f, err := Parse([]byte(templated))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		app, stage, key, want string
	}{
		{"SV-API_GW_MGT", "DEV", "envRepo", "sda/services/SV-API_GW_MGT/DEV"},
		{"portal", "PROD", "envRepo", "sda/applications/CUSTOMER_PORTAL/PROD"},
		{"portal", "PROD", "path", "apps/portal/claim.yaml"},
		{"portal", "PROD", "mapping", "mappings/portal.yaml"},
		{"portal", "PROD", "targetBranch", "release"},
		{"legacy", "PROD", "environment", "Production"},
	} {
		if got := f.DriverConfig(tc.app, tc.stage)[tc.key]; got != tc.want {
			t.Errorf("%s/%s %s = %v, want %s", tc.app, tc.stage, tc.key, got, tc.want)
		}
	}
	if f.Apps["legacy"].Deploy.Driver != "octopus" || f.Apps["portal"].Deploy.Driver != "crossplane" {
		t.Errorf("driver override: %+v", f.Apps)
	}
	if _, err := Parse([]byte(strings.Replace(templated, "template: service }", "template: nope }", 1))); err == nil {
		t.Error("expected unknown template error")
	}
}
