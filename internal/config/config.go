package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a YAML duration such as "5s" or "2m".
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	parsed, err := time.ParseDuration(value.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", value.Value, err)
	}
	d.Duration = parsed
	return nil
}

type Config struct {
	Agent    AgentConfig    `yaml:"agent"`
	Signals  SignalsConfig  `yaml:"signals"`
	Reporter ReporterConfig `yaml:"reporter"`
}

type AgentConfig struct {
	SampleInterval    Duration       `yaml:"sample_interval"`
	HeartbeatInterval Duration       `yaml:"heartbeat_interval"`
	StaleAfter        Duration       `yaml:"stale_after"`
	FailClosed        bool           `yaml:"fail_closed"`
	Debounce          DebounceConfig `yaml:"debounce"`
}

type DebounceConfig struct {
	GameSamples        int `yaml:"game_samples"`
	InteractiveSamples int `yaml:"interactive_samples"`
	IdleSamples        int `yaml:"idle_samples"`
}

type SignalsConfig struct {
	Logind      LogindConfig      `yaml:"logind"`
	GameProcess GameProcessConfig `yaml:"game_process"`
	NVIDIA      NVIDIAConfig      `yaml:"nvidia"`
}

type LogindConfig struct {
	Enabled        bool     `yaml:"enabled"`
	Command        string   `yaml:"command"`
	GraphicalTypes []string `yaml:"graphical_types"`
	Timeout        Duration `yaml:"timeout"`
}

type GameProcessConfig struct {
	Enabled             bool     `yaml:"enabled"`
	ProcRoot            string   `yaml:"proc_root"`
	Names               []string `yaml:"names"`
	CommandLineContains []string `yaml:"command_line_contains"`
}

type NVIDIAConfig struct {
	Enabled          bool     `yaml:"enabled"`
	Command          string   `yaml:"command"`
	UtilizationFloor int      `yaml:"utilization_floor_percent"`
	Timeout          Duration `yaml:"timeout"`
}

type ReporterConfig struct {
	Mode       string           `yaml:"mode"`
	Kubernetes KubernetesConfig `yaml:"kubernetes"`
}

type KubernetesConfig struct {
	APIServer string   `yaml:"api_server"`
	Namespace string   `yaml:"namespace"`
	Name      string   `yaml:"name"`
	TokenFile string   `yaml:"token_file"`
	CAFile    string   `yaml:"ca_file"`
	Timeout   Duration `yaml:"timeout"`
}

func Defaults() Config {
	return Config{
		Agent: AgentConfig{
			SampleInterval:    Duration{5 * time.Second},
			HeartbeatInterval: Duration{15 * time.Second},
			StaleAfter:        Duration{60 * time.Second},
			FailClosed:        true,
			Debounce: DebounceConfig{
				GameSamples:        1,
				InteractiveSamples: 2,
				IdleSamples:        3,
			},
		},
		Signals: SignalsConfig{
			Logind: LogindConfig{
				Enabled:        true,
				Command:        "/usr/bin/loginctl",
				GraphicalTypes: []string{"wayland", "x11"},
				Timeout:        Duration{3 * time.Second},
			},
			GameProcess: GameProcessConfig{ProcRoot: "/proc"},
			NVIDIA: NVIDIAConfig{
				Command:          "/usr/bin/nvidia-smi",
				UtilizationFloor: 20,
				Timeout:          Duration{3 * time.Second},
			},
		},
		Reporter: ReporterConfig{
			Mode:       "stdout",
			Kubernetes: KubernetesConfig{Timeout: Duration{10 * time.Second}},
		},
	}
}

func Load(path string) (Config, error) {
	cfg := Defaults()
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read configuration: %w", err)
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode configuration: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.Agent.SampleInterval.Duration <= 0 {
		return fmt.Errorf("agent.sample_interval must be positive")
	}
	if c.Agent.HeartbeatInterval.Duration < c.Agent.SampleInterval.Duration {
		return fmt.Errorf("agent.heartbeat_interval must be at least sample_interval")
	}
	if c.Agent.StaleAfter.Duration < c.Agent.SampleInterval.Duration {
		return fmt.Errorf("agent.stale_after must be at least sample_interval")
	}
	if c.Agent.Debounce.GameSamples < 1 || c.Agent.Debounce.InteractiveSamples < 1 || c.Agent.Debounce.IdleSamples < 1 {
		return fmt.Errorf("all agent.debounce sample counts must be at least one")
	}
	if !c.Agent.FailClosed {
		return fmt.Errorf("agent.fail_closed=false is not supported in phase 1")
	}
	if !c.Signals.Logind.Enabled && !c.Signals.GameProcess.Enabled && !c.Signals.NVIDIA.Enabled {
		return fmt.Errorf("at least one signal provider must be enabled")
	}
	if c.Signals.Logind.Enabled {
		if err := validateCommand(c.Signals.Logind.Command, "signals.logind.command"); err != nil {
			return err
		}
		if len(c.Signals.Logind.GraphicalTypes) == 0 {
			return fmt.Errorf("signals.logind.graphical_types cannot be empty")
		}
		if c.Signals.Logind.Timeout.Duration <= 0 {
			return fmt.Errorf("signals.logind.timeout must be positive")
		}
	}
	if c.Signals.GameProcess.Enabled {
		if !filepath.IsAbs(c.Signals.GameProcess.ProcRoot) {
			return fmt.Errorf("signals.game_process.proc_root must be absolute")
		}
		if len(c.Signals.GameProcess.Names) == 0 && len(c.Signals.GameProcess.CommandLineContains) == 0 {
			return fmt.Errorf("game process matching requires at least one name or command-line literal")
		}
		for _, value := range append(append([]string{}, c.Signals.GameProcess.Names...), c.Signals.GameProcess.CommandLineContains...) {
			if value == "" || len(value) > 256 || strings.ContainsRune(value, '\x00') {
				return fmt.Errorf("game process matches must be non-empty literals no longer than 256 bytes")
			}
		}
	}
	if c.Signals.NVIDIA.Enabled {
		if err := validateCommand(c.Signals.NVIDIA.Command, "signals.nvidia.command"); err != nil {
			return err
		}
		if c.Signals.NVIDIA.UtilizationFloor < 1 || c.Signals.NVIDIA.UtilizationFloor > 100 {
			return fmt.Errorf("signals.nvidia.utilization_floor_percent must be between 1 and 100")
		}
		if c.Signals.NVIDIA.Timeout.Duration <= 0 {
			return fmt.Errorf("signals.nvidia.timeout must be positive")
		}
	}
	switch c.Reporter.Mode {
	case "stdout":
	case "kubernetes":
		if err := c.Reporter.Kubernetes.Validate(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("reporter.mode must be stdout or kubernetes")
	}
	return nil
}

func validateCommand(command, field string) error {
	if !filepath.IsAbs(command) {
		return fmt.Errorf("%s must be an absolute path", field)
	}
	return nil
}

func (c KubernetesConfig) Validate() error {
	parsed, err := url.Parse(c.APIServer)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return fmt.Errorf("reporter.kubernetes.api_server must be an HTTPS origin without a path")
	}
	if !dnsLabel(c.Namespace) || !dnsLabel(c.Name) {
		return fmt.Errorf("reporter.kubernetes namespace and name must be DNS labels")
	}
	if !filepath.IsAbs(c.TokenFile) || !filepath.IsAbs(c.CAFile) {
		return fmt.Errorf("reporter.kubernetes token_file and ca_file must be absolute")
	}
	if c.Timeout.Duration <= 0 {
		return fmt.Errorf("reporter.kubernetes.timeout must be positive")
	}
	return nil
}

func dnsLabel(value string) bool {
	if len(value) < 1 || len(value) > 63 || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}
