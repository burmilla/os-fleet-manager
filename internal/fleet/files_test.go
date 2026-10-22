package fleet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "fleet.yml")
}

func TestSplitFiles(t *testing.T) {
	path := writeFiles(t, map[string]string{
		"fleet.yml":          "defaults: {os_version: v3.0.0}\nservers: {solo: {os_version: v2.0.3}}\n",
		"swarms/a.yml":       "upgrade_policy: auto\nservers:\n  a1: {docker_version: docker-29.9.1}\n",
		"swarms/a/prod.yml":  "os_version: v3.0.1\nservers:\n  a2: {max_uptime: 7d}\n",
		"swarms/b/test.yaml": "os_version: v3.0.2\n",
		"swarms/README.md":   "not a config file",
		"swarms/.hidden.yml": "garbage: [",
		"swarms/b/notes.txt": "ignored",
		"swarms/a/empty.yml": "",
	})
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, host := range []string{"a1", "a2"} {
		d := cfg.Resolve(host, "a", "prod", now)
		if !d.Known || d.Swarm != "a" || d.Environment != "prod" || d.OSVersion != "v3.0.1" || d.UpgradePolicy != PolicyAuto {
			t.Fatalf("%s: %+v", host, d)
		}
	}
	if d := cfg.Resolve("a1", "a", "prod", now); d.DockerVersion != "docker-29.9.1" {
		t.Fatalf("swarm level server entry: %+v", d)
	}
	if d := cfg.Resolve("a2", "a", "empty", now); d.Known || d.MaxUptimeSeconds != 0 {
		t.Fatalf("environment server entry applies only in its environment: %+v", d)
	}
	if d := cfg.Resolve("solo", "a", "prod", now); d.Known || d.OSVersion != "v3.0.1" {
		t.Fatalf("top level server entry applies only outside Swarms: %+v", d)
	}
	if d := cfg.Resolve("solo", "", "", now); !d.Known || d.OSVersion != "v2.0.3" {
		t.Fatalf("server outside Swarms: %+v", d)
	}
	if d := cfg.Resolve("x", "b", "test", now); d.OSVersion != "v3.0.2" {
		t.Fatalf("swarm b only defined by its environment directory: %+v", d)
	}
	if _, ok := cfg.Swarms["a"].Environments["empty"]; !ok {
		t.Fatal("empty environment file should define the environment")
	}
	if got := cfg.Listed(); got["a2"] != [2]string{"a", "prod"} || got["solo"] != [2]string{} {
		t.Fatalf("listed servers: %v", got)
	}
}

func TestSplitFilesConflicts(t *testing.T) {
	for name, c := range map[string]struct {
		files map[string]string
		err   string
	}{
		"swarm twice": {map[string]string{
			"fleet.yml":    "swarms: {a: {}}\n",
			"swarms/a.yml": "upgrade_policy: auto\n",
		}, "swarms/a.yml: swarm a is already defined"},
		"environment twice": {map[string]string{
			"fleet.yml":         "",
			"swarms/a.yml":      "environments: {prod: {}}\n",
			"swarms/a/prod.yml": "os_version: v3.0.1\n",
		}, "swarms/a/prod.yml: environment prod of swarm a is already defined"},
		"old server membership": {map[string]string{
			"fleet.yml":    "",
			"swarms/a.yml": "servers: {s1: {environment: prod}}\n",
		}, "field environment not found"},
		"bad name": {map[string]string{
			"fleet.yml": "swarms: {a: {environments: {\"x/y\": {}}}}\n",
		}, "invalid"},
		"unknown field": {map[string]string{
			"fleet.yml":    "",
			"swarms/a.yml": "os_verison: v3.0.0\n",
		}, "swarms/a.yml: yaml: unmarshal errors"},
		"validation": {map[string]string{
			"fleet.yml":         "",
			"swarms/a/prod.yml": "upgrade_policy: sometimes\n",
		}, `swarms.a.environments.prod: invalid upgrade_policy "sometimes"`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeFiles(t, c.files))
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Fatalf("expected %q, got %v", c.err, err)
			}
		})
	}
}
