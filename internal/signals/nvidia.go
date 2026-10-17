package signals

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
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
	// A resident compute app is a game at any utilization, including 0% and
	// non-numeric readings. Utilization with no compute app stays idle.
	appsOutput, err := p.Runner.Run(ctx, p.Command, "--query-compute-apps=pid,process_name,used_gpu_memory", "--format=csv,noheader,nounits")
	if err != nil {
		return ActivityUnknown, "", fmt.Errorf("query NVIDIA compute apps: %w", err)
	}
	apps, err := parseComputeApps(string(appsOutput))
	if err != nil {
		return ActivityUnknown, "", err
	}
	maximum, numeric := maximumUtilization(values)
	if len(apps) > 0 {
		return ActivityGame, computeAppReason(apps, utilizationDetail(numeric, maximum, p.UtilizationFloor)), nil
	}
	if !numeric {
		return ActivityIdle, "gpu utilization unavailable", nil
	}
	if maximum >= p.UtilizationFloor {
		return ActivityIdle, fmt.Sprintf("GPU utilization %d%% meets %d%% floor without compute apps", maximum, p.UtilizationFloor), nil
	}
	return ActivityIdle, fmt.Sprintf("GPU utilization %d%% below %d%% floor", maximum, p.UtilizationFloor), nil
}

func maximumUtilization(values []int) (int, bool) {
	if len(values) == 0 {
		return 0, false
	}
	maximum := 0
	for _, value := range values {
		if value > maximum {
			maximum = value
		}
	}
	return maximum, true
}

func utilizationDetail(numeric bool, maximum, floor int) string {
	if !numeric {
		return "utilization unavailable"
	}
	if maximum >= floor {
		return fmt.Sprintf("utilization %d%% meeting %d%% floor", maximum, floor)
	}
	return fmt.Sprintf("utilization %d%% below %d%% floor", maximum, floor)
}

func computeAppReason(apps []computeApp, utilization string) string {
	app, extra := selectComputeApp(apps)
	if extra > 0 {
		return fmt.Sprintf("GPU compute app %q (pid %d) and %d more with %s", app.Name, app.PID, extra, utilization)
	}
	return fmt.Sprintf("GPU compute app %q (pid %d) with %s", app.Name, app.PID, utilization)
}

type computeApp struct {
	PID    int
	Name   string
	Memory int
}

func parseUtilization(output string) ([]int, error) {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return nil, fmt.Errorf("NVIDIA utilization query returned no values")
	}
	var values []int
	hasReading := false
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), "%"))
		if line == "" {
			continue
		}
		hasReading = true
		if isUnavailable(line) {
			continue
		}
		value, err := strconv.Atoi(line)
		if err != nil || value < 0 || value > 100 {
			return nil, fmt.Errorf("invalid NVIDIA utilization %q", line)
		}
		values = append(values, value)
	}
	if !hasReading {
		return nil, fmt.Errorf("NVIDIA utilization query returned no values")
	}
	return values, nil
}

func parseComputeApps(output string) ([]computeApp, error) {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return nil, nil
	}
	var apps []computeApp
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		app, err := parseComputeApp(line)
		if err != nil {
			return nil, err
		}
		apps = append(apps, app)
	}
	return apps, nil
}

func parseComputeApp(line string) (computeApp, error) {
	parts := strings.Split(line, ",")
	if len(parts) < 3 {
		return computeApp{}, fmt.Errorf("invalid NVIDIA compute app %q", shorten(line, 128))
	}
	pid, err := parseComputePID(strings.TrimSpace(parts[0]))
	if err != nil {
		return computeApp{}, err
	}
	name, err := displayComputeAppName(strings.Join(parts[1:len(parts)-1], ","))
	if err != nil {
		return computeApp{}, err
	}
	memory, err := parseComputeMemory(strings.TrimSpace(parts[len(parts)-1]))
	if err != nil {
		return computeApp{}, err
	}
	return computeApp{PID: pid, Name: name, Memory: memory}, nil
}

func parseComputePID(field string) (int, error) {
	if field == "" || len(field) > 10 {
		return 0, fmt.Errorf("invalid NVIDIA compute app pid %q", shorten(field, 32))
	}
	for _, r := range field {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("invalid NVIDIA compute app pid %q", shorten(field, 32))
		}
	}
	pid, err := strconv.Atoi(field)
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("invalid NVIDIA compute app pid %q", field)
	}
	return pid, nil
}

func displayComputeAppName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", fmt.Errorf("invalid NVIDIA compute app name")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("invalid NVIDIA compute app name %q", shorten(name, 128))
		}
	}
	if len(name) > 256 {
		name = shorten(name, 256)
	}
	return name, nil
}

func parseComputeMemory(field string) (int, error) {
	if isUnavailable(field) {
		return -1, nil
	}
	lower := strings.ToLower(field)
	for _, suffix := range []string{"mib", "mb"} {
		if strings.HasSuffix(lower, suffix) {
			field = strings.TrimSpace(field[:len(field)-len(suffix)])
			break
		}
	}
	if field == "" || len(field) > 10 {
		return 0, fmt.Errorf("invalid NVIDIA compute app memory %q", shorten(field, 32))
	}
	for _, r := range field {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("invalid NVIDIA compute app memory %q", shorten(field, 32))
		}
	}
	value, err := strconv.Atoi(field)
	if err != nil {
		return 0, fmt.Errorf("invalid NVIDIA compute app memory %q", shorten(field, 32))
	}
	return value, nil
}

func selectComputeApp(apps []computeApp) (computeApp, int) {
	best := 0
	for i := 1; i < len(apps); i++ {
		if betterComputeApp(apps[i], apps[best]) {
			best = i
		}
	}
	return apps[best], len(apps) - 1
}

func betterComputeApp(candidate, current computeApp) bool {
	if candidate.Memory != current.Memory {
		return candidate.Memory > current.Memory
	}
	if candidate.PID != current.PID {
		return candidate.PID < current.PID
	}
	return candidate.Name < current.Name
}

func isUnavailable(reading string) bool {
	switch strings.ToLower(strings.TrimSpace(reading)) {
	case "n/a", "[n/a]", "[not supported]":
		return true
	default:
		return false
	}
}

func shorten(value string, max int) string {
	if len(value) <= max {
		return value
	}
	cut := value[:max]
	for !utf8.ValidString(cut) && len(cut) > 0 {
		cut = cut[:len(cut)-1]
	}
	return cut
}
