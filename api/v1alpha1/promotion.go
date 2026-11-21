// Package v1alpha1 contains the ship.dev/v1alpha1 API: the Promotion custom resource.
//
// A Promotion is a front-end for a ledger merge request: ship-controller turns it
// into an MR and mirrors GitLab state into .status. The ledger stays the source of
// truth; .status is a cache (docs/design.md §12).
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	GroupVersion  = schema.GroupVersion{Group: "ship.dev", Version: "v1alpha1"}
	SchemeBuilder = runtime.NewSchemeBuilder(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &Promotion{}, &PromotionList{})
		metav1.AddToGroupVersion(s, GroupVersion)
		return nil
	})
	AddToScheme = SchemeBuilder.AddToScheme
)

// Finalizer closes the ledger MR when a Promotion is deleted before it merged.
const Finalizer = "ship.dev/close-ledger-mr"

type Phase string

const (
	PhasePending          Phase = "Pending"
	PhaseProposed         Phase = "Proposed"
	PhaseAwaitingApproval Phase = "AwaitingApproval"
	PhaseDeploying        Phase = "Deploying"
	PhaseSucceeded        Phase = "Succeeded"
	PhaseFailed           Phase = "Failed"
	PhaseRejected         Phase = "Rejected"
	PhaseAbandoned        Phase = "Abandoned"
	PhaseSuperseded       Phase = "Superseded"
)

func (p Phase) Terminal() bool {
	switch p {
	case PhaseSucceeded, PhaseFailed, PhaseRejected, PhaseAbandoned, PhaseSuperseded:
		return true
	}
	return false
}

// Condition types.
const (
	CondProposed  = "Proposed"
	CondValidated = "Validated"
	CondApproved  = "Approved" // V8 satisfied
	CondMerged    = "Merged"
	CondDeployed  = "Deployed"
)

type Promotion struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              PromotionSpec   `json:"spec"`
	Status            PromotionStatus `json:"status,omitempty"`
}

type PromotionSpec struct {
	App         string `json:"app"`
	Stage       string `json:"stage"`
	Freight     string `json:"freight"` // registry/repo@sha256:… or sha256:…
	Reason      string `json:"reason,omitempty"`
	RequestedBy string `json:"requestedBy"` // admission policy: must equal the creating user
}

type MRRef struct {
	IID        int      `json:"iid"`
	URL        string   `json:"url"`
	State      string   `json:"state,omitempty"`
	ApprovedBy []string `json:"approvedBy,omitempty"`
}

type PipelineRef struct {
	ID     int    `json:"id"`
	URL    string `json:"url"`
	Status string `json:"status"`
}

type Target struct {
	Kind       string   `json:"kind"` // envMR, …
	URL        string   `json:"url,omitempty"`
	State      string   `json:"state,omitempty"`
	ApprovedBy []string `json:"approvedBy,omitempty"`
}

type PromotionStatus struct {
	Phase              Phase              `json:"phase,omitempty"`
	Message            string             `json:"message,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Freight            string             `json:"freight,omitempty"` // resolved reference
	Branch             string             `json:"branch,omitempty"`
	LedgerMR           *MRRef             `json:"ledgerMR,omitempty"`
	LedgerCommit       string             `json:"ledgerCommit,omitempty"`
	DeployPipeline     *PipelineRef       `json:"deployPipeline,omitempty"`
	Targets            []Target           `json:"targets,omitempty"`
	Violations         []string           `json:"violations,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

type PromotionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Promotion `json:"items"`
}

// ---- deep copy (hand-written; the types are small) ----

func (in *Promotion) DeepCopyInto(out *Promotion) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	in.Status.DeepCopyInto(&out.Status)
}

func (in *Promotion) DeepCopy() *Promotion {
	if in == nil {
		return nil
	}
	out := new(Promotion)
	in.DeepCopyInto(out)
	return out
}

func (in *Promotion) DeepCopyObject() runtime.Object { return in.DeepCopy() }

func (in *PromotionStatus) DeepCopyInto(out *PromotionStatus) {
	*out = *in
	if in.LedgerMR != nil {
		m := *in.LedgerMR
		m.ApprovedBy = append([]string(nil), in.LedgerMR.ApprovedBy...)
		out.LedgerMR = &m
	}
	if in.DeployPipeline != nil {
		p := *in.DeployPipeline
		out.DeployPipeline = &p
	}
	if in.Targets != nil {
		out.Targets = make([]Target, len(in.Targets))
		for i, t := range in.Targets {
			t.ApprovedBy = append([]string(nil), t.ApprovedBy...)
			out.Targets[i] = t
		}
	}
	out.Violations = append([]string(nil), in.Violations...)
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *PromotionList) DeepCopyInto(out *PromotionList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]Promotion, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *PromotionList) DeepCopy() *PromotionList {
	if in == nil {
		return nil
	}
	out := new(PromotionList)
	in.DeepCopyInto(out)
	return out
}

func (in *PromotionList) DeepCopyObject() runtime.Object { return in.DeepCopy() }

func (in *PromotionStatus) DeepCopy() *PromotionStatus {
	if in == nil {
		return nil
	}
	out := new(PromotionStatus)
	in.DeepCopyInto(out)
	return out
}
