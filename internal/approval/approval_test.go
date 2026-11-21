package approval

import (
	"strings"
	"testing"

	"github.com/jbadeau/ship/internal/config"
)

func cfg(min int) *config.File {
	return &config.File{
		GitLab:     config.GitLabConfig{BotUsername: "ship-bot", PromoterUsername: "ship-promoter", ApproverUsername: "ship-approver", LedgerMinApprovals: min},
		Identities: map[string]string{"alice@bank.example": "alice"},
	}
}

func TestV8(t *testing.T) {
	for _, tc := range []struct {
		name string
		min  int
		f    Facts
		ok   bool
		want string
	}{
		{"independent", 1, Facts{Author: "ship-promoter", Requesters: []string{"alice@bank.example"}, Approvers: []string{"bob"}}, true, ""},
		{"self approval via identity mapping", 1, Facts{Author: "ship-promoter", Requesters: []string{"alice@bank.example"}, Approvers: []string{"alice"}}, false, "alice (requester"},
		{"author", 1, Facts{Author: "promoter", Approvers: []string{"promoter"}}, false, "MR author"},
		{"bot", 1, Facts{Author: "x", Approvers: []string{"ship-approver"}}, false, "service account"},
		{"case insensitive", 1, Facts{Author: "x", Requesters: []string{"Bob"}, Approvers: []string{"bob"}}, false, "requester"},
		{"self + other counts other", 1, Facts{Author: "x", Requesters: []string{"alice"}, Approvers: []string{"alice", "carol"}}, true, ""},
		{"two required", 2, Facts{Author: "x", Approvers: []string{"carol", "dave"}}, true, ""},
		{"two required, one self", 2, Facts{Author: "x", Requesters: []string{"dave"}, Approvers: []string{"carol", "dave"}}, false, "1 independent"},
		{"none", 1, Facts{Author: "x"}, false, "0 independent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Check(cfg(tc.min), tc.f)
			if r.OK() != tc.ok || !strings.Contains(r.Violation, tc.want) {
				t.Fatalf("ok=%v violation=%q", r.OK(), r.Violation)
			}
		})
	}
}
