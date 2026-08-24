// Package v1alpha1 defines the Phase 1 wire model for NodeActivity status.
// The CRD and controller are deliberately deferred to Phase 2.
package v1alpha1

import "time"

const (
	Group    = "availability.interactive-node.io"
	Version  = "v1alpha1"
	Kind     = "NodeActivity"
	Resource = "nodeactivities"
)

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

// NodeActivityStatus is the only Phase 1 API payload. Phase 2 may add spec and
// condition fields when it introduces the versioned CRD.
type NodeActivityStatus struct {
	State       State     `json:"state"`
	Activity    Activity  `json:"activity"`
	Reason      string    `json:"reason"`
	ObservedAt  time.Time `json:"observedAt"`
	HeartbeatAt time.Time `json:"heartbeatAt"`
	Generation  int64     `json:"generation"`
	FailClosed  bool      `json:"failClosed"`
}
