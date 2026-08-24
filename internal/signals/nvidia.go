package signals

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type NVIDIAProvider struct {
	Command          string
	UtilizationFloor int
	Timeout          time.Duration
	Runner           Runner
}

func (NVIDIAProvider) Name() string { return "nvidia" }

func (p NVIDIAProvider) Observe(ctx context.Context) (Activity, string, error) {
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	output, err := p.Runner.Run(ctx, p.Command, "--query-gpu=utilization.gpu", "--format=csv,noheader,nounits")
	if err != nil {
		return ActivityUnknown, "", fmt.Errorf("query NVIDIA utilization: %w", err)
	}
	values, err := parseUtilization(string(output))
	if err != nil {
		return ActivityUnknown, "", err
	}
	maximum := 0
	for _, value := range values {
		if value > maximum {
			maximum = value
		}
	}
	if maximum >= p.UtilizationFloor {
		return ActivityGame, fmt.Sprintf("GPU utilization %d%% meets %d%% floor", maximum, p.UtilizationFloor), nil
	}
	return ActivityIdle, fmt.Sprintf("GPU utilization %d%% below %d%% floor", maximum, p.UtilizationFloor), nil
}

func parseUtilization(output string) ([]int, error) {
	var values []int
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), "%"))
		if line == "" {
			continue
		}
		value, err := strconv.Atoi(line)
		if err != nil || value < 0 || value > 100 {
			return nil, fmt.Errorf("invalid NVIDIA utilization %q", line)
		}
		values = append(values, value)
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("NVIDIA utilization query returned no values")
	}
	return values, nil
}
