package fleet

import (
	"slices"
	"testing"
	"time"
)

func TestExampleConfig(t *testing.T) {
	cfg, err := Load("../../examples/fleet-config/fleet.yml")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 24, 0, 30, 0, 0, time.UTC) // Sat 03:30 Helsinki

	d := cfg.Resolve("swarm-a-mgr-02", "swarm-a", "production", now)
	if !d.Known || d.Swarm != "swarm-a" || d.Environment != "production" {
		t.Fatalf("server entry of swarm-a production: %+v", d)
	}
	if d.OSVersion != "v3.0.0" || d.DockerVersion != "docker-29.9.1" || d.UpgradePolicy != PolicyAuto {
		t.Fatalf("unexpected versions: %+v", d)
	}
	if !d.MaintenanceWindowActive || d.MaintenanceWindowEnds != "2026-10-24T01:00:00Z" {
		t.Fatalf("window should be active until 04:00 Helsinki: %+v", d)
	}
	if d.CheckIntervalSeconds != 300 || d.MaxConcurrentMaintenance != 1 {
		t.Fatalf("unexpected defaults: %+v", d)
	}

	// Membership comes only from the client: the same hostname calling
	// another path does not get the settings of swarm-a production.
	if d := cfg.Resolve("swarm-a-mgr-02", "swarm-a", "test", now); d.Known || d.OSVersion != "v3.0.1" {
		t.Fatalf("server entry must not apply outside its environment: %+v", d)
	}
	if d := cfg.Resolve("swarm-a-mgr-02", "", "", now); d.Known || d.Swarm != "" {
		t.Fatalf("server entry must not apply outside its Swarm: %+v", d)
	}

	d = cfg.Resolve("swarm-a-mgr-01", "swarm-a", "production", now)
	if d.MaintenanceWindowActive || d.MaintenanceWindowNextStart != "2026-10-31T00:00:00Z" {
		// Summer time ends on 25 Oct, so 02:00 Helsinki is 00:00 UTC.
		t.Fatalf("mgr-01 window is over for this week: %+v", d)
	}

	d = cfg.Resolve("worker-17", "swarm-a", "test", now)
	if d.Known || d.OSVersion != "v3.0.1" || d.MaxConcurrentMaintenance != 2 {
		t.Fatalf("unknown server should use swarm environment settings: %+v", d)
	}

	d = cfg.Resolve("standalone", "", "", now)
	if d.UpgradePolicy != PolicyDownload || d.OSVersion != "v3.0.0" {
		t.Fatalf("unexpected defaults: %+v", d)
	}
	if cfg.Resolve("legacy-01", "", "", now).UpgradePolicy != PolicyNone {
		t.Fatal("legacy-01 should be pinned")
	}
}

func TestNoWindowMeansAlwaysActive(t *testing.T) {
	cfg, err := Parse([]byte("defaults: {os_version: v3.0.0}"))
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.Resolve("x", "", "", time.Now())
	if !d.MaintenanceWindowActive || d.UpgradePolicy != PolicyNone || d.OSImage != DefaultOSImage {
		t.Fatalf("%+v", d)
	}
}

func TestWindowAcrossMidnight(t *testing.T) {
	w := &MaintenanceWindow{Days: []string{"friday"}, Start: "23:00", Duration: "3h", Timezone: "UTC"}
	if err := w.Validate(); err != nil {
		t.Fatal(err)
	}
	sat0130 := time.Date(2026, 10, 24, 1, 30, 0, 0, time.UTC)
	end, ok := w.ActiveUntil(sat0130)
	if !ok || !end.Equal(time.Date(2026, 10, 24, 2, 0, 0, 0, time.UTC)) {
		t.Fatalf("expected active until 02:00, got %v %v", end, ok)
	}
	if _, ok := w.ActiveUntil(sat0130.Add(time.Hour)); ok {
		t.Fatal("window should be closed at 02:30")
	}
	next, _ := w.NextStart(sat0130)
	if !next.Equal(time.Date(2026, 10, 30, 23, 0, 0, 0, time.UTC)) {
		t.Fatalf("unexpected next start %v", next)
	}
}

func TestValidation(t *testing.T) {
	bad := []string{
		"defaults: {upgrade_policy: always}",
		"defaults: {docker_version: 29.9.2}",
		"defaults: {check_interval: 1s}",
		"defaults: {max_uptime: 10m}",
		"defaults: {restart_after: tomorrow}",
		"defaults: {maintenance_window: {start: '25:00', duration: 1h}}",
		"defaults: {maintenance_window: {start: '02:00', duration: 1h, days: [funday]}}",
		"defaults: {maintenance_window: {start: '02:00', duration: 1h, timezone: Mars/Base}}",
		"defaults: {os_versoin: v3.0.0}",
		"servers: {a: {max_concurrent_maintenance: -1}}",
	}
	for _, b := range bad {
		if _, err := Parse([]byte(b)); err == nil {
			t.Errorf("expected error for %s", b)
		}
	}
}

func TestEvaluate(t *testing.T) {
	cfg, err := Parse([]byte(`
defaults: {os_version: v3.0.0, docker_version: docker-29.9.2, max_uptime: 30d}
servers:
  a: {restart_after: "2026-10-20T00:00:00Z"}
`))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)

	d := cfg.Resolve("b", "", "", now)
	d.Evaluate("v3.0.0", "docker-29.9.2", now.Add(-24*time.Hour), now)
	if d.MaintenanceRequired || d.MaxUptimeSeconds != 30*24*3600 {
		t.Fatalf("nothing to do expected: %+v", d)
	}
	d = cfg.Resolve("b", "", "", now)
	d.Evaluate("v2.0.3", "docker-29.9.2", now.Add(-31*24*time.Hour), now)
	if !d.RestartRequired || len(d.MaintenanceReasons) != 2 || d.MaintenanceReasons[0] != ReasonOSUpgrade {
		t.Fatalf("os upgrade and restart expected: %+v", d)
	}
	d = cfg.Resolve("a", "", "", now)
	d.Evaluate("v3.0.0", "docker-29.9.2", now.Add(-5*24*time.Hour), now)
	if !d.RestartRequired {
		t.Fatalf("one-off restart expected: %+v", d)
	}
	d = cfg.Resolve("a", "", "", now)
	d.Evaluate("v3.0.0", "docker-29.9.2", now.Add(-time.Hour), now)
	if d.RestartRequired {
		t.Fatalf("already restarted after restart_after: %+v", d)
	}
	if _, err := ParseDuration("1.5d"); err == nil {
		t.Fatal("fractional days should be rejected")
	}
}

func TestSSHSettings(t *testing.T) {
	cfg, err := Load("../../examples/fleet-config/fleet.yml")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()

	prod := cfg.Resolve("swarm-a-mgr-01", "swarm-a", "production", now)
	if len(prod.SSH.AuthorizedKeys) != 2 || prod.SSH.PasswordAuthentication == nil || *prod.SSH.PasswordAuthentication {
		t.Fatalf("production should inherit keys and disable passwords: %+v", prod.SSH)
	}
	test := cfg.Resolve("worker-17", "swarm-a", "test", now)
	if len(test.SSH.AuthorizedKeys) != 3 || !*test.SSH.PasswordAuthentication {
		t.Fatalf("test should replace keys and allow passwords: %+v", test.SSH)
	}
	if d := cfg.Resolve("standalone", "", "", now); d.SSH.PasswordAuthentication != nil {
		t.Fatalf("password authentication is not managed by default: %+v", d.SSH)
	}

	off, on := false, true
	keys := slices.Clone(prod.SSH.AuthorizedKeys)
	slices.Reverse(keys)
	prod.CheckSSH(&SSH{AuthorizedKeys: keys, PasswordAuthentication: &off})
	if !prod.SSHInSync {
		t.Fatal("key order must not matter")
	}
	prod.CheckSSH(&SSH{AuthorizedKeys: keys, PasswordAuthentication: &on})
	if prod.SSHInSync {
		t.Fatal("password authentication differs")
	}
	prod.CheckSSH(&SSH{AuthorizedKeys: keys[:1], PasswordAuthentication: &off})
	if prod.SSHInSync {
		t.Fatal("a key is missing")
	}
	prod.CheckSSH(nil)
	if prod.SSHInSync {
		t.Fatal("client did not report SSH state")
	}

	if prod.SSH.DisableAfterBoot != "10m0s" || test.SSH.DisableAfterBoot != "0s" {
		t.Fatalf("production disables SSH 10m after boot, test turns it off: %q %q", prod.SSH.DisableAfterBoot, test.SSH.DisableAfterBoot)
	}

	// An empty list removes all managed keys, which differs from unset.
	cfg, err = Parse([]byte("defaults: {ssh: {authorized_keys: []}}"))
	if err != nil {
		t.Fatal(err)
	}
	if d := cfg.Resolve("x", "", "", now); d.SSH.AuthorizedKeys == nil || len(d.SSH.AuthorizedKeys) != 0 {
		t.Fatalf("empty key list must be kept: %#v", d.SSH)
	}
}

func TestInvalidSSHKey(t *testing.T) {
	for _, c := range []string{
		"defaults: {ssh: {authorized_keys: [not-a-key]}}",
		"defaults: {ssh: {disable_after_boot: 30s}}",
		"defaults: {ssh: {authorized_keys: [\"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOiuTuZpapVCjAUUGcXj2zMLBbrr0QyMsca1UEx+EBWQ a\\nssh-rsa x\"]}}",
	} {
		if _, err := Parse([]byte(c)); err == nil {
			t.Fatalf("expected error for %s", c)
		}
	}
}
