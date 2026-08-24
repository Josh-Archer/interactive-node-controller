// Package v1alpha1 defines the public NodeActivity API.
package v1alpha1

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	Group    = "availability.interactive-node.io"
	Version  = "v1alpha1"
	Kind     = "NodeActivity"
	Resource = "nodeactivities"
)

var GroupVersion = schema.GroupVersion{Group: Group, Version: Version}

var SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

var AddToScheme = SchemeBuilder.AddToScheme

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion, &NodeActivity{}, &NodeActivityList{})
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}

type State string

const (
	StateActive  State = "active"
	StateIdle    State = "idle"
	StateUnknown State = "unknown"
	StateStale   State = "stale"
)

type Activity string

const (
	ActivityGame        Activity = "game"
	ActivityInteractive Activity = "interactive"
	ActivityIdle        Activity = "idle"
	ActivityUnknown     Activity = "unknown"
)

// NodeActivitySpec is Git-owned enrollment data. The host reporter has no
// permission to alter it; it writes only the status subresource.
type NodeActivitySpec struct {
	// NodeName is the exact Kubernetes Node whose interactive availability is
	// managed for this enrollment.
	NodeName string `json:"nodeName"`
}

// ManagedTaintStatus makes the controller's dynamic ownership observable
// without representing the Node's complete taint set.
type ManagedTaintStatus struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Effect string `json:"effect"`
}

// NodeActivityStatus is host-reported state plus controller-owned conditions.
// The reporter uses a JSON merge patch, so absent controller fields are
// preserved while it refreshes the host-owned fields.
type NodeActivityStatus struct {
	State        State               `json:"state"`
	Activity     Activity            `json:"activity"`
	Reason       string              `json:"reason"`
	ObservedAt   time.Time           `json:"observedAt"`
	HeartbeatAt  time.Time           `json:"heartbeatAt"`
	Generation   int64               `json:"generation"`
	FailClosed   bool                `json:"failClosed"`
	Conditions   []metav1.Condition  `json:"conditions,omitempty"`
	ManagedTaint *ManagedTaintStatus `json:"managedTaint,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type NodeActivity struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              NodeActivitySpec   `json:"spec,omitempty"`
	Status            NodeActivityStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type NodeActivityList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeActivity `json:"items"`
}

func (in *NodeActivity) DeepCopyInto(out *NodeActivity) {
	*out = *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	if in.Status.Conditions != nil {
		out.Status.Conditions = make([]metav1.Condition, len(in.Status.Conditions))
		copy(out.Status.Conditions, in.Status.Conditions)
	}
	if in.Status.ManagedTaint != nil {
		out.Status.ManagedTaint = new(ManagedTaintStatus)
		*out.Status.ManagedTaint = *in.Status.ManagedTaint
	}
}

func (in *NodeActivity) DeepCopy() *NodeActivity {
	if in == nil {
		return nil
	}
	out := new(NodeActivity)
	in.DeepCopyInto(out)
	return out
}

func (in *NodeActivity) DeepCopyObject() runtime.Object { return in.DeepCopy() }

func (in *NodeActivityList) DeepCopyInto(out *NodeActivityList) {
	*out = *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]NodeActivity, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *NodeActivityList) DeepCopy() *NodeActivityList {
	if in == nil {
		return nil
	}
	out := new(NodeActivityList)
	in.DeepCopyInto(out)
	return out
}

func (in *NodeActivityList) DeepCopyObject() runtime.Object { return in.DeepCopy() }
