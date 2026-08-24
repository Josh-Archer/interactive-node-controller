package reporter

import (
	"context"

	v1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
)

type State = v1alpha1.State
type Status = v1alpha1.NodeActivityStatus

const (
	StateActive  = v1alpha1.StateActive
	StateIdle    = v1alpha1.StateIdle
	StateUnknown = v1alpha1.StateUnknown
	StateStale   = v1alpha1.StateStale
)

type Reporter interface {
	Report(context.Context, Status) error
}
