package signals

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestProcessProviderDetectsGamesFromFakeProc(t *testing.T) {
	tests := []struct {
		name, comm, cmdline, executable string
		want                            Activity
	}{
		{name: "desktop session only", comm: "gnome-shell", cmdline: "gnome-shell", want: ActivityIdle},
		{name: "Steam reaper AppId", comm: "reaper", cmdline: "reaper SteamLaunch AppId=12345 -- /games/game.exe", want: ActivityGame},
		{name: "SteamLaunch child", comm: "reaper", cmdline: "reaper SteamLaunch 12345 -- game", want: ActivityGame},
		{name: "gamescope", comm: "gamescope", cmdline: "gamescope -- steam", want: ActivityGame},
		{name: "wine process", comm: "wine64-preloader", cmdline: "wine64-preloader /games/game.exe", want: ActivityGame},
		{name: "Proton game preloader executable", comm: "game.exe", cmdline: "/games/game.exe", executable: "/steam/compatibilitytools.d/Proton/files/bin/wine-preloader", want: ActivityGame},
		{name: "Proton command", comm: "python", cmdline: "/steam/compatibilitytools.d/Proton/run waitforexitandrun game.exe", want: ActivityGame},
		{name: "configured process name", comm: "custom-game", cmdline: "custom-game", want: ActivityGame},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeFakeProcess(t, root, i+100, tt.comm, tt.cmdline, tt.executable)
			names := map[string]struct{}{}
			if tt.name == "configured process name" {
				names["custom-game"] = struct{}{}
			}
			activity, _, err := (ProcessProvider{ProcRoot: root, Names: names}).Observe(context.Background())
			if err != nil {
				t.Fatalf("Observe() error = %v", err)
			}
			if activity != tt.want {
				t.Fatalf("Observe() activity = %q, want %q", activity, tt.want)
			}
		})
	}
}

func TestProcessProviderIgnoresUnrelatedSteamProcess(t *testing.T) {
	root := t.TempDir()
	writeFakeProcess(t, root, 200, "steam", "steam", "")
	activity, _, err := (ProcessProvider{ProcRoot: root}).Observe(context.Background())
	if err != nil || activity != ActivityIdle {
		t.Fatalf("Observe() = (%q, %v), want idle", activity, err)
	}
}

func writeFakeProcess(t *testing.T, root string, pid int, comm, cmdline, executable string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.ReplaceAll(cmdline, " ", "\x00")), 0o600); err != nil {
		t.Fatal(err)
	}
	if executable != "" {
		if err := os.Symlink(executable, filepath.Join(dir, "exe")); err != nil {
			t.Fatal(err)
		}
	}
}
