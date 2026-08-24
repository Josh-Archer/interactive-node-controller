package signals

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type ProcessProvider struct {
	ProcRoot            string
	Names               map[string]struct{}
	CommandLineContains []string
}

func (ProcessProvider) Name() string { return "game-process" }

func (p ProcessProvider) Observe(_ context.Context) (Activity, string, error) {
	entries, err := os.ReadDir(p.ProcRoot)
	if err != nil {
		return ActivityUnknown, "", fmt.Errorf("read proc root: %w", err)
	}
	readable := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		processRoot := filepath.Join(p.ProcRoot, entry.Name())
		comm, commErr := readLimited(filepath.Join(processRoot, "comm"), 4096)
		cmdline, cmdErr := readLimited(filepath.Join(processRoot, "cmdline"), 64<<10)
		if commErr != nil && cmdErr != nil {
			continue // Processes can disappear while /proc is being scanned.
		}
		readable++
		name := strings.TrimSpace(string(comm))
		if _, matched := p.Names[name]; matched {
			return ActivityGame, fmt.Sprintf("process name %q matched (pid %s)", name, entry.Name()), nil
		}
		commandLine := strings.ReplaceAll(string(cmdline), "\x00", " ")
		for _, literal := range p.CommandLineContains {
			if strings.Contains(commandLine, literal) {
				return ActivityGame, fmt.Sprintf("command-line literal %q matched (pid %s)", literal, entry.Name()), nil
			}
		}
	}
	if readable == 0 {
		return ActivityUnknown, "", fmt.Errorf("no readable processes in %s", p.ProcRoot)
	}
	return ActivityIdle, "no configured game process matched", nil
}

func readLimited(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, limit))
}
