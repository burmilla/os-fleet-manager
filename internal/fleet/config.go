// Package fleet contains the fleet configuration schema which is read from
// the configuration Git repository and the logic to resolve the desired
// state of a single server from it.
package fleet

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	PolicyAuto     = "auto"
	PolicyDownload = "download"
	PolicyNone     = "none"

	DefaultOSImage       = "burmilla/os"
	DefaultCheckInterval = 5 * time.Minute
)

// Settings can be defined on every level of the configuration:
// defaults < swarm < swarm environment < server.
// Empty values are inherited from the level above.
type Settings struct {
	OSImage           string             `yaml:"os_image,omitempty"`
	OSVersion         string             `yaml:"os_version,omitempty"`
	DockerVersion     string             `yaml:"docker_version,omitempty"`
	UpgradePolicy     string             `yaml:"upgrade_policy,omitempty"`
	CheckInterval     string             `yaml:"check_interval,omitempty"`
	MaintenanceWindow *MaintenanceWindow `yaml:"maintenance_window,omitempty"`

	// MaxConcurrentMaintenance limits how many servers of one Swarm +
	// Environment can hold a maintenance lock (upgrade or restart) at once.
	MaxConcurrentMaintenance int `yaml:"max_concurrent_maintenance,omitempty"`
	// MaxUptime schedules a restart in the maintenance window when the
	// server has been up longer than this (e.g. 720h or 30d).
	MaxUptime string `yaml:"max_uptime,omitempty"`
	// RestartAfter requests a one-off restart: servers booted before this
	// time (RFC 3339) restart in their next maintenance window.
	RestartAfter string `yaml:"restart_after,omitempty"`
}

type Swarm struct {
	Settings     `yaml:",inline"`
	Environments map[string]Settings `yaml:"environments,omitempty"`
}

type Server struct {
	Settings    `yaml:",inline"`
	Swarm       string `yaml:"swarm,omitempty"`
	Environment string `yaml:"environment,omitempty"`
}

// Config is the content of fleet.yml.
type Config struct {
	Defaults Settings          `yaml:"defaults"`
	Swarms   map[string]Swarm  `yaml:"swarms,omitempty"`
	Servers  map[string]Server `yaml:"servers,omitempty"`

	// Revision is the Git commit the configuration was loaded from.
	Revision string `yaml:"-"`
}

// Load reads and validates a fleet configuration file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

func Parse(data []byte) (*Config, error) {
	cfg := &Config{}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse fleet config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) Validate() error {
	check := func(where string, s Settings) error {
		switch s.UpgradePolicy {
		case "", PolicyAuto, PolicyDownload, PolicyNone:
		default:
			return fmt.Errorf("%s: invalid upgrade_policy %q (auto, download or none)", where, s.UpgradePolicy)
		}
		if s.MaxConcurrentMaintenance < 0 {
			return fmt.Errorf("%s: max_concurrent_maintenance cannot be negative", where)
		}
		if s.MaxUptime != "" {
			if d, err := ParseDuration(s.MaxUptime); err != nil || d < time.Hour {
				return fmt.Errorf("%s: invalid max_uptime %q (e.g. 720h or 30d, at least 1h)", where, s.MaxUptime)
			}
		}
		if s.RestartAfter != "" {
			if _, err := time.Parse(time.RFC3339, s.RestartAfter); err != nil {
				return fmt.Errorf("%s: invalid restart_after %q (RFC 3339, e.g. 2026-10-24T00:00:00Z)", where, s.RestartAfter)
			}
		}
		if s.CheckInterval != "" {
			d, err := time.ParseDuration(s.CheckInterval)
			if err != nil || d < 10*time.Second {
				return fmt.Errorf("%s: invalid check_interval %q (Go duration, at least 10s)", where, s.CheckInterval)
			}
		}
		if s.DockerVersion != "" && !strings.HasPrefix(s.DockerVersion, "docker-") {
			return fmt.Errorf("%s: docker_version %q must be an engine name like docker-29.9.2", where, s.DockerVersion)
		}
		if s.MaintenanceWindow != nil {
			if err := s.MaintenanceWindow.Validate(); err != nil {
				return fmt.Errorf("%s: maintenance_window: %w", where, err)
			}
		}
		return nil
	}

	if err := check("defaults", c.Defaults); err != nil {
		return err
	}
	for name, sw := range c.Swarms {
		if err := check("swarms."+name, sw.Settings); err != nil {
			return err
		}
		for env, s := range sw.Environments {
			if err := check("swarms."+name+".environments."+env, s); err != nil {
				return err
			}
		}
	}
	for name, srv := range c.Servers {
		if err := check("servers."+name, srv.Settings); err != nil {
			return err
		}
	}
	return nil
}

// Desired is the resolved configuration of one server. It is also the
// response body of GET /api/v1/config.
type Desired struct {
	Hostname                   string             `json:"hostname"`
	Swarm                      string             `json:"swarm"`
	Environment                string             `json:"environment"`
	Known                      bool               `json:"known"`
	OSImage                    string             `json:"os_image"`
	OSVersion                  string             `json:"os_version"`
	DockerVersion              string             `json:"docker_version"`
	UpgradePolicy              string             `json:"upgrade_policy"`
	CheckIntervalSeconds       int                `json:"check_interval_seconds"`
	MaintenanceWindow          *MaintenanceWindow `json:"maintenance_window"`
	MaintenanceWindowActive    bool               `json:"maintenance_window_active"`
	MaintenanceWindowEnds      string             `json:"maintenance_window_ends"`
	MaintenanceWindowNextStart string             `json:"maintenance_window_next_start"`
	MaxConcurrentMaintenance   int                `json:"max_concurrent_maintenance"`
	MaxUptimeSeconds           int64              `json:"max_uptime_seconds"`
	RestartAfter               string             `json:"restart_after"`
	ConfigRevision             string             `json:"config_revision"`

	// Filled in from the last report of the server.
	RestartRequired     bool     `json:"restart_required"`
	MaintenanceRequired bool     `json:"maintenance_required"`
	MaintenanceReasons  []string `json:"maintenance_reasons"`
}

const (
	ReasonOSUpgrade     = "os_upgrade"
	ReasonDockerUpgrade = "docker_upgrade"
	ReasonRestart       = "restart"
)

// Evaluate fills in what maintenance the server needs based on the state
// it reported.
func (d *Desired) Evaluate(osVersion, dockerVersion string, bootTime, now time.Time) {
	d.MaintenanceReasons = []string{}
	if d.OSVersion != "" && d.OSVersion != osVersion {
		d.MaintenanceReasons = append(d.MaintenanceReasons, ReasonOSUpgrade)
	}
	if d.DockerVersion != "" && d.DockerVersion != dockerVersion {
		d.MaintenanceReasons = append(d.MaintenanceReasons, ReasonDockerUpgrade)
	}
	if !bootTime.IsZero() {
		if d.MaxUptimeSeconds > 0 && now.Sub(bootTime) > time.Duration(d.MaxUptimeSeconds)*time.Second {
			d.RestartRequired = true
		}
		if t, err := time.Parse(time.RFC3339, d.RestartAfter); err == nil && bootTime.Before(t) && !now.Before(t) {
			d.RestartRequired = true
		}
	}
	if d.RestartRequired {
		d.MaintenanceReasons = append(d.MaintenanceReasons, ReasonRestart)
	}
	d.MaintenanceRequired = len(d.MaintenanceReasons) > 0
}

// ParseDuration is time.ParseDuration with support for a "d" (days) suffix.
func ParseDuration(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		var days int
		if _, err := fmt.Sscanf(n, "%d", &days); err != nil || fmt.Sprint(days) != n {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

func merge(dst *Settings, src Settings) {
	if src.OSImage != "" {
		dst.OSImage = src.OSImage
	}
	if src.OSVersion != "" {
		dst.OSVersion = src.OSVersion
	}
	if src.DockerVersion != "" {
		dst.DockerVersion = src.DockerVersion
	}
	if src.UpgradePolicy != "" {
		dst.UpgradePolicy = src.UpgradePolicy
	}
	if src.MaxConcurrentMaintenance != 0 {
		dst.MaxConcurrentMaintenance = src.MaxConcurrentMaintenance
	}
	if src.MaxUptime != "" {
		dst.MaxUptime = src.MaxUptime
	}
	if src.RestartAfter != "" {
		dst.RestartAfter = src.RestartAfter
	}
	if src.CheckInterval != "" {
		dst.CheckInterval = src.CheckInterval
	}
	if src.MaintenanceWindow != nil {
		dst.MaintenanceWindow = src.MaintenanceWindow
	}
}

// Resolve returns the desired state of a server. swarm and environment are
// the values reported by the client; a servers: entry overrides them.
func (c *Config) Resolve(hostname, swarm, environment string, now time.Time) Desired {
	srv, known := c.Servers[hostname]
	if known {
		if srv.Swarm != "" {
			swarm = srv.Swarm
		}
		if srv.Environment != "" {
			environment = srv.Environment
		}
	}

	s := Settings{
		OSImage:                  DefaultOSImage,
		UpgradePolicy:            PolicyNone,
		MaxConcurrentMaintenance: 1,
		CheckInterval:            DefaultCheckInterval.String(),
	}
	merge(&s, c.Defaults)
	if sw, ok := c.Swarms[swarm]; ok {
		merge(&s, sw.Settings)
		if env, ok := sw.Environments[environment]; ok {
			merge(&s, env)
		}
	}
	if known {
		merge(&s, srv.Settings)
	}

	interval, _ := time.ParseDuration(s.CheckInterval)
	maxUptime, _ := ParseDuration(s.MaxUptime)
	d := Desired{
		Hostname:                 hostname,
		Swarm:                    swarm,
		Environment:              environment,
		Known:                    known,
		OSImage:                  s.OSImage,
		OSVersion:                s.OSVersion,
		DockerVersion:            s.DockerVersion,
		UpgradePolicy:            s.UpgradePolicy,
		CheckIntervalSeconds:     int(interval.Seconds()),
		MaintenanceWindow:        s.MaintenanceWindow,
		MaxConcurrentMaintenance: s.MaxConcurrentMaintenance,
		MaxUptimeSeconds:         int64(maxUptime.Seconds()),
		RestartAfter:             s.RestartAfter,
		ConfigRevision:           c.Revision,
		MaintenanceReasons:       []string{},
	}
	if w := s.MaintenanceWindow; w != nil {
		if end, ok := w.ActiveUntil(now); ok {
			d.MaintenanceWindowActive = true
			d.MaintenanceWindowEnds = end.UTC().Format(time.RFC3339)
		}
		if next, ok := w.NextStart(now); ok {
			d.MaintenanceWindowNextStart = next.UTC().Format(time.RFC3339)
		}
	} else {
		d.MaintenanceWindowActive = true
	}
	return d
}
