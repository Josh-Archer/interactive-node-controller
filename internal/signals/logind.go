package signals

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type LogindProvider struct {
	Command        string
	GraphicalTypes map[string]struct{}
	Timeout        time.Duration
	Runner         Runner
}

func (LogindProvider) Name() string { return "logind" }

func (p LogindProvider) Observe(ctx context.Context) (Activity, string, error) {
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	output, err := p.Runner.Run(ctx, p.Command, "list-sessions", "--no-legend", "--no-pager")
	if err != nil {
		return ActivityUnknown, "", fmt.Errorf("list logind sessions: %w", err)
	}
	sessions := parseSessionIDs(string(output))
	for _, session := range sessions {
		properties, err := p.Runner.Run(ctx, p.Command, "show-session", session, "--property=Active", "--property=Type", "--no-pager")
		if err != nil {
			return ActivityUnknown, "", fmt.Errorf("inspect logind session %s: %w", session, err)
		}
		active, sessionType := parseSessionProperties(string(properties))
		_, graphical := p.GraphicalTypes[strings.ToLower(sessionType)]
		if active && graphical {
			return ActivityInteractive, fmt.Sprintf("active %s session %s", sessionType, session), nil
		}
	}
	return ActivityIdle, "no active graphical logind session", nil
}

func parseSessionIDs(output string) []string {
	var sessions []string
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && safeSessionID(fields[0]) {
			sessions = append(sessions, fields[0])
		}
	}
	return sessions
}

func safeSessionID(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	if first := value[0]; (first < 'a' || first > 'z') && (first < 'A' || first > 'Z') && (first < '0' || first > '9') {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func parseSessionProperties(output string) (bool, string) {
	var active bool
	var sessionType string
	for _, line := range strings.Split(output, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		switch key {
		case "Active":
			active = strings.EqualFold(value, "yes")
		case "Type":
			sessionType = strings.ToLower(value)
		}
	}
	return active, sessionType
}
