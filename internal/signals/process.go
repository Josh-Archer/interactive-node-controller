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
		executable, _ := os.Readlink(filepath.Join(processRoot, "exe"))
		if _, matched := p.Names[name]; matched {
			return ActivityGame, fmt.Sprintf("process name %q matched (pid %s)", name, entry.Name()), nil
		}
		commandLine := strings.ReplaceAll(string(cmdline), "\x00", " ")
		if matched, reason := builtInGameMatch(name, commandLine, executable); matched {
			return ActivityGame, fmt.Sprintf("%s (pid %s)", reason, entry.Name()), nil
		}
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

func builtInGameMatch(name, commandLine, executable string) (bool, string) {
	lowerName := strings.ToLower(name)
	lowerCommand := strings.ToLower(commandLine)
	lowerExecutable := strings.ToLower(filepath.Base(executable))
	if lowerName == "gamescope" || strings.Contains(lowerCommand, "gamescope --") {
		return true, "gamescope process detected"
	}
	if lowerName == "reaper" && strings.Contains(lowerCommand, "appid=") {
		return true, "Steam reaper with AppId detected"
	}
	if strings.Contains(lowerCommand, "steamlaunch") && (lowerName == "reaper" || strings.Contains(lowerCommand, "steam") && strings.Contains(lowerCommand, "reaper")) {
		return true, "Steam SteamLaunch child detected"
	}
	if lowerName == "wine" || lowerName == "wine64" || lowerName == "wine-preloader" || lowerName == "wine64-preloader" || lowerName == "proton" || lowerName == "proton-preloader" || strings.HasPrefix(lowerExecutable, "wine") {
		return true, fmt.Sprintf("Proton/Wine game process %q detected", name)
	}
	if strings.Contains(lowerCommand, "/proton") && (strings.Contains(lowerCommand, "run") || strings.Contains(lowerCommand, "waitforexitandrun")) {
		return true, "Proton game launch detected"
	}
	return false, ""
}

func readLimited(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, limit))
}
