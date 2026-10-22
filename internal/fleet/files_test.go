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
		"fleet.yml":          "defaults: {os_version: v3.0.0}\n",
		"swarms/a.yml":       "upgrade_policy: auto\nservers:\n  a1: {environment: prod}\n",
		"swarms/a/prod.yml":  "os_version: v3.0.1\nservers:\n  a2: {}\n",
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
	for host, want := range map[string]string{"a1": "v3.0.1", "a2": "v3.0.1"} {
		d := cfg.Resolve(host, "", "", now)
		if d.Swarm != "a" || d.Environment != "prod" || d.OSVersion != want || d.UpgradePolicy != PolicyAuto {
			t.Fatalf("%s: %+v", host, d)
		}
	}
	if d := cfg.Resolve("x", "b", "test", now); d.OSVersion != "v3.0.2" {
		t.Fatalf("swarm b only defined by its environment directory: %+v", d)
	}
	if _, ok := cfg.Swarms["a"].Environments["empty"]; !ok {
		t.Fatal("empty environment file should define the environment")
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
		"server twice": {map[string]string{
			"fleet.yml":    "servers: {s1: {}}\n",
			"swarms/a.yml": "servers: {s1: {}}\n",
		}, "swarms/a.yml: server s1 is already defined in fleet.yml"},
		"server in two teams": {map[string]string{
			"fleet.yml":    "",
			"swarms/a.yml": "servers: {s1: {}}\n",
			"swarms/b.yml": "servers: {s1: {}}\n",
		}, "swarms/b.yml: server s1 is already defined in swarms/a.yml"},
		"move to other swarm": {map[string]string{
			"fleet.yml":    "",
			"swarms/a.yml": "servers: {s1: {swarm: b}}\n",
		}, `swarms/a.yml: server s1 cannot be moved to swarm "b" from here`},
		"move to other environment": {map[string]string{
			"fleet.yml":         "",
			"swarms/a/prod.yml": "servers: {s1: {environment: test}}\n",
		}, `swarms/a/prod.yml: server s1 cannot be moved to environment "test" from here`},
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
