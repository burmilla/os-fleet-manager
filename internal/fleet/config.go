// Package fleet contains the fleet configuration schema which is read from
// the configuration Git repository and the logic to resolve the desired
// state of a single server from it.
package fleet

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"
)

const (
	PolicyAuto     = "auto"
	PolicyDownload = "download"
	PolicyNone     = "none"

	DefaultOSImage       = "burmilla/os"
	DefaultCheckInterval = 5 * time.Minute

	// NodeTypeNomadQEMU is reported by the fleet-client of Nomad QEMU nodes
	// (https://github.com/olljanat/nomad-qemu-node). Servers without a
	// node type run BurmillaOS.
	NodeTypeNomadQEMU = "nomad-qemu-node"
	// DefaultNomadQEMUImage is the os_image of Nomad QEMU nodes.
	DefaultNomadQEMUImage = "ghcr.io/olljanat/nomad-qemu-node"
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
	// SSH access of the rancher and docker users. Changes are applied by
	// fleet-client right away, they do not need a maintenance lock.
	SSH *SSH `yaml:"ssh,omitempty"`
	// ConsoleAccess enables (true) or disables (false) interactive access
	// to the console: sshd and the tty logins (ros console access). The
	// console container keeps running for User Docker. Applied right away,
	// kept across reboots. nil is not managed.
	ConsoleAccess *bool `yaml:"console_access,omitempty"`
}

// SSH settings. Unset fields are not managed by fleet-client.
type SSH struct {
	// AuthorizedKeys replaces the keys which fleet-client manages in
	// authorized_keys. Keys added by other means (cloud-config, by hand)
	// are left alone. An empty list removes all managed keys.
	AuthorizedKeys []string `yaml:"authorized_keys" json:"authorized_keys"`
	// PasswordAuthentication enables or disables password logins in sshd.
	PasswordAuthentication *bool `yaml:"password_authentication,omitempty" json:"password_authentication"`
	// DisableAfterBoot hardens the server by stopping sshd once it has
	// been up this long (e.g. 10m) and no SSH session is active. sshd
	// starts again after a reboot. "0" enables sshd right away.
	DisableAfterBoot string `yaml:"disable_after_boot,omitempty" json:"disable_after_boot,omitempty"`
	// Disabled is reported by the client: sshd is disabled.
	Disabled bool `yaml:"-" json:"disabled,omitempty"`
}

// Which Swarm and environment a server belongs to is decided by the
// server itself (the API path its client uses), never by the
// configuration. servers: entries only adjust settings of servers which
// are members of the Swarm or environment they are listed in, so a team
// can only affect its own servers.

type Swarm struct {
	Settings `yaml:",inline"`
	// Standalone marks a group of servers which are not members of a
	// Docker Swarm. It has environments, htpasswd and maintenance slots
	// like a Swarm, but the health check before and after maintenance
	// only looks at the server itself.
	Standalone   bool                   `yaml:"standalone,omitempty"`
	Environments map[string]Environment `yaml:"environments,omitempty"`
	// Servers adjusts settings of members of this Swarm by hostname.
	Servers map[string]Settings `yaml:"servers,omitempty"`
}

type Environment struct {
	Settings `yaml:",inline"`
	// Htpasswd lines (user:bcrypt-hash, e.g. from htpasswd -nbB) which
	// protect this Swarm + Environment. Servers in it must authenticate
	// with one of them.
	Htpasswd []string `yaml:"htpasswd,omitempty"`
	// Servers adjusts settings of members of this environment by hostname.
	Servers map[string]Settings `yaml:"servers,omitempty"`
}

// Config is the content of fleet.yml.
type Config struct {
	Defaults Settings         `yaml:"defaults"`
	Swarms   map[string]Swarm `yaml:"swarms,omitempty"`
	// Servers adjusts settings of servers which are not in any Swarm.
	Servers map[string]Settings `yaml:"servers,omitempty"`

	// Revision is the Git commit the configuration was loaded from.
	Revision string `yaml:"-"`
}

// Load reads a fleet configuration file together with the Swarm files in
// the swarms directory next to it, and validates the result.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	if err := decode(data, cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if err := cfg.loadSwarmDir(filepath.Join(filepath.Dir(path), SwarmDir)); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// decode parses YAML strictly; an empty document is valid.
func decode(data []byte, v any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func Parse(data []byte) (*Config, error) {
	cfg := &Config{}
	if err := decode(data, cfg); err != nil {
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
		if s.SSH != nil {
			for _, key := range s.SSH.AuthorizedKeys {
				if strings.ContainsAny(key, "\r\n") {
					return fmt.Errorf("%s: ssh.authorized_keys entries must be single lines", where)
				}
				if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(key)); err != nil {
					return fmt.Errorf("%s: invalid ssh.authorized_keys entry %q: %w", where, key, err)
				}
			}
			if v := s.SSH.DisableAfterBoot; v != "" {
				if d, err := time.ParseDuration(v); err != nil || (d != 0 && d < time.Minute) {
					return fmt.Errorf("%s: invalid ssh.disable_after_boot %q (Go duration, at least 1m, 0 = off)", where, v)
				}
			}
		}
		if s.MaintenanceWindow != nil {
			if err := s.MaintenanceWindow.Validate(); err != nil {
				return fmt.Errorf("%s: maintenance_window: %w", where, err)
			}
		}
		return nil
	}

	users := map[string]string{}
	if err := check("defaults", c.Defaults); err != nil {
		return err
	}
	checkServers := func(where string, servers map[string]Settings) error {
		for h, srv := range servers {
			if err := check(where+".servers."+h, srv); err != nil {
				return err
			}
		}
		return nil
	}
	for name, sw := range c.Swarms {
		if err := validName(name); err != nil {
			return fmt.Errorf("swarms.%s: %w", name, err)
		}
		if err := check("swarms."+name, sw.Settings); err != nil {
			return err
		}
		if err := checkServers("swarms."+name, sw.Servers); err != nil {
			return err
		}
		for env, e := range sw.Environments {
			where := "swarms." + name + ".environments." + env
			if err := validName(env); err != nil {
				return fmt.Errorf("%s: %w", where, err)
			}
			if err := check(where, e.Settings); err != nil {
				return err
			}
			if err := checkServers(where, e.Servers); err != nil {
				return err
			}
			for _, line := range e.Htpasswd {
				user, hash, ok := strings.Cut(line, ":")
				if !ok || user == "" || !strings.HasPrefix(hash, "$2") {
					return fmt.Errorf("%s: htpasswd entries must be user:bcrypt-hash (htpasswd -nbB user password)", where)
				}
				if other, dup := users[user]; dup {
					return fmt.Errorf("%s: htpasswd user %q is already used by %s", where, user, other)
				}
				users[user] = name + "/" + env
			}
		}
	}
	return checkServers("", c.Servers)
}

// validName: Swarm and environment names are part of API paths and file
// names.
func validName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\?#%") {
		return fmt.Errorf("invalid name %q", name)
	}
	return nil
}

// Desired is the resolved configuration of one server. It is also the
// response body of GET /api/v1/config.
type Desired struct {
	Hostname                   string             `json:"hostname"`
	Swarm                      string             `json:"swarm"`
	Environment                string             `json:"environment"`
	NodeType                   string             `json:"node_type,omitempty"`
	Known                      bool               `json:"known"`
	Standalone                 bool               `json:"standalone"`
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
	SSH                        SSH                `json:"ssh"`
	ConsoleAccess              *bool              `json:"console_access"`
	ConfigRevision             string             `json:"config_revision"`

	// Filled in from the last report of the server.
	SSHInSync           bool     `json:"ssh_in_sync"`
	ConsoleAccessInSync bool     `json:"console_access_in_sync"`
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

// CheckSSH compares the SSH state reported by the server with the
// desired one. Settings which are not managed are ignored.
func (d *Desired) CheckSSH(reported *SSH) {
	d.SSHInSync = true
	if d.SSH.AuthorizedKeys != nil {
		if reported == nil || reported.AuthorizedKeys == nil {
			d.SSHInSync = false
		} else {
			want, got := slices.Clone(d.SSH.AuthorizedKeys), slices.Clone(reported.AuthorizedKeys)
			for i := range want {
				want[i] = strings.TrimSpace(want[i])
			}
			for i := range got {
				got[i] = strings.TrimSpace(got[i])
			}
			slices.Sort(want)
			slices.Sort(got)
			d.SSHInSync = slices.Equal(slices.Compact(want), slices.Compact(got))
		}
	}
	if p := d.SSH.PasswordAuthentication; p != nil {
		if reported == nil || reported.PasswordAuthentication == nil || *reported.PasswordAuthentication != *p {
			d.SSHInSync = false
		}
	}
}

// CheckConsoleAccess compares the console access state reported by the
// server with the desired one. Unmanaged is always in sync.
func (d *Desired) CheckConsoleAccess(reported *bool) {
	d.ConsoleAccessInSync = d.ConsoleAccess == nil || (reported != nil && *reported == *d.ConsoleAccess)
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
	if src.SSH != nil {
		ssh := SSH{}
		if dst.SSH != nil {
			ssh = *dst.SSH
		}
		if src.SSH.AuthorizedKeys != nil {
			ssh.AuthorizedKeys = src.SSH.AuthorizedKeys
		}
		if src.SSH.PasswordAuthentication != nil {
			ssh.PasswordAuthentication = src.SSH.PasswordAuthentication
		}
		if src.SSH.DisableAfterBoot != "" {
			ssh.DisableAfterBoot = src.SSH.DisableAfterBoot
		}
		dst.SSH = &ssh
	}
	if src.ConsoleAccess != nil {
		dst.ConsoleAccess = src.ConsoleAccess
	}
}

// Resolve returns the desired state of a BurmillaOS server which is a
// member of the given Swarm and environment (empty: not in a Swarm).
func (c *Config) Resolve(hostname, swarm, environment string, now time.Time) Desired {
	return c.ResolveNode(hostname, swarm, environment, "", now)
}

// ResolveNode is Resolve for a server of the given node type. Nomad QEMU
// nodes do not take os_image and os_version from defaults (they describe
// BurmillaOS), do not run User Docker and are never members of a Docker
// Swarm, so their group is handled like a standalone one.
func (c *Config) ResolveNode(hostname, swarm, environment, nodeType string, now time.Time) Desired {
	nomad := nodeType == NodeTypeNomadQEMU
	s := Settings{
		OSImage:                  DefaultOSImage,
		UpgradePolicy:            PolicyNone,
		MaxConcurrentMaintenance: 1,
		CheckInterval:            DefaultCheckInterval.String(),
	}
	defaults := c.Defaults
	if nomad {
		s.OSImage = DefaultNomadQEMUImage
		defaults.OSImage, defaults.OSVersion = "", ""
	}
	merge(&s, defaults)
	known := false
	if swarm == "" {
		if srv, ok := c.Servers[hostname]; ok {
			merge(&s, srv)
			known = true
		}
	} else if sw, ok := c.Swarms[swarm]; ok {
		merge(&s, sw.Settings)
		env, envOK := sw.Environments[environment]
		if envOK {
			merge(&s, env.Settings)
		}
		if srv, ok := sw.Servers[hostname]; ok {
			merge(&s, srv)
			known = true
		}
		if srv, ok := env.Servers[hostname]; envOK && ok {
			merge(&s, srv)
			known = true
		}
	}

	if nomad {
		s.DockerVersion = ""
	}

	interval, _ := time.ParseDuration(s.CheckInterval)
	maxUptime, _ := ParseDuration(s.MaxUptime)
	d := Desired{
		Hostname:                 hostname,
		Swarm:                    swarm,
		Environment:              environment,
		NodeType:                 nodeType,
		Known:                    known,
		Standalone:               c.Swarms[swarm].Standalone || (nomad && swarm != ""),
		OSImage:                  s.OSImage,
		OSVersion:                s.OSVersion,
		DockerVersion:            s.DockerVersion,
		UpgradePolicy:            s.UpgradePolicy,
		CheckIntervalSeconds:     int(interval.Seconds()),
		MaintenanceWindow:        s.MaintenanceWindow,
		MaxConcurrentMaintenance: s.MaxConcurrentMaintenance,
		MaxUptimeSeconds:         int64(maxUptime.Seconds()),
		RestartAfter:             s.RestartAfter,
		ConsoleAccess:            s.ConsoleAccess,
		ConfigRevision:           c.Revision,
		SSHInSync:                true,
		ConsoleAccessInSync:      true,
		MaintenanceReasons:       []string{},
	}
	if s.SSH != nil {
		d.SSH = *s.SSH
		if v, err := time.ParseDuration(d.SSH.DisableAfterBoot); err == nil {
			d.SSH.DisableAfterBoot = v.String()
		}
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

// Credential is one htpasswd entry of a Swarm + Environment.
type Credential struct {
	Swarm, Environment, User, Hash string
}

// Credentials returns all htpasswd entries by user name.
func (c *Config) Credentials() map[string]Credential {
	out := map[string]Credential{}
	for name, sw := range c.Swarms {
		for env, e := range sw.Environments {
			for _, line := range e.Htpasswd {
				user, hash, _ := strings.Cut(line, ":")
				out[user] = Credential{Swarm: name, Environment: env, User: user, Hash: hash}
			}
		}
	}
	return out
}

// Listed returns the servers listed in the configuration with the Swarm
// and environment they are listed in, for status pages. A hostname listed
// in several places is reported once.
func (c *Config) Listed() map[string][2]string {
	out := map[string][2]string{}
	add := func(h, swarm, env string) {
		if _, ok := out[h]; !ok {
			out[h] = [2]string{swarm, env}
		}
	}
	for h := range c.Servers {
		add(h, "", "")
	}
	names := make([]string, 0, len(c.Swarms))
	for name := range c.Swarms {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		sw := c.Swarms[name]
		envs := make([]string, 0, len(sw.Environments))
		for env := range sw.Environments {
			envs = append(envs, env)
		}
		slices.Sort(envs)
		for _, env := range envs {
			for h := range sw.Environments[env].Servers {
				add(h, name, env)
			}
		}
		for h := range sw.Servers {
			add(h, name, "")
		}
	}
	return out
}

// Protected reports whether a Swarm + Environment requires authentication.
func (c *Config) Protected(swarm, environment string) bool {
	return len(c.Swarms[swarm].Environments[environment].Htpasswd) > 0
}
