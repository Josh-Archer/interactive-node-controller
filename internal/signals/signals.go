package signals

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
)

type Activity string

const (
	ActivityUnknown     Activity = "unknown"
	ActivityIdle        Activity = "idle"
	ActivityInteractive Activity = "interactive"
	ActivityGame        Activity = "game"
)

type Observation struct {
	Provider string
	Activity Activity
	Reason   string
	Err      error
}

type Provider interface {
	Name() string
	Observe(context.Context) (Activity, string, error)
}

type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, command string, args ...string) ([]byte, error) {
	// command comes from validated absolute-path configuration and args are fixed
	// by providers. No shell is involved.
	cmd := exec.CommandContext(ctx, command, args...)
	output := &boundedBuffer{remaining: 1 << 20}
	cmd.Stdout = output
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

type boundedBuffer struct {
	bytes.Buffer
	remaining int
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	if len(data) > b.remaining {
		written, _ := b.Buffer.Write(data[:b.remaining])
		b.remaining = 0
		return written, errors.New("command output exceeds 1 MiB limit")
	}
	written, err := b.Buffer.Write(data)
	b.remaining -= written
	return written, err
}

var _ io.Writer = (*boundedBuffer)(nil)

func Collect(ctx context.Context, providers []Provider) []Observation {
	observations := make([]Observation, 0, len(providers))
	for _, provider := range providers {
		activity, reason, err := provider.Observe(ctx)
		if err != nil {
			activity = ActivityUnknown
			reason = err.Error()
		}
		observations = append(observations, Observation{
			Provider: provider.Name(),
			Activity: activity,
			Reason:   reason,
			Err:      err,
		})
	}
	return observations
}

// Aggregate applies positive-signal precedence game > interactive > idle.
// Idle requires every enabled provider to make a healthy idle observation;
// uncertainty therefore fails closed as unknown.
func Aggregate(observations []Observation) (Activity, string) {
	if len(observations) == 0 {
		return ActivityUnknown, "no signal providers configured"
	}
	sorted := append([]Observation(nil), observations...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Provider < sorted[j].Provider })

	for _, target := range []Activity{ActivityGame, ActivityInteractive} {
		for _, observation := range sorted {
			if observation.Activity == target {
				return target, fmt.Sprintf("%s: %s", observation.Provider, observation.Reason)
			}
		}
	}
	unknownReasons := make([]string, 0)
	idleReasons := make([]string, 0)
	for _, observation := range sorted {
		switch observation.Activity {
		case ActivityUnknown:
			unknownReasons = append(unknownReasons, fmt.Sprintf("%s: %s", observation.Provider, observation.Reason))
		case ActivityIdle:
			idleReasons = append(idleReasons, fmt.Sprintf("%s: %s", observation.Provider, observation.Reason))
		default:
			unknownReasons = append(unknownReasons, fmt.Sprintf("%s: invalid activity %q", observation.Provider, observation.Activity))
		}
	}
	if len(unknownReasons) > 0 {
		return ActivityUnknown, strings.Join(unknownReasons, "; ")
	}
	return ActivityIdle, strings.Join(idleReasons, "; ")
}
