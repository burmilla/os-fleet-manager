package fleet

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SwarmDir is the directory next to fleet.yml with one file per Swarm
// (swarms/<swarm>.yml) and optionally one file per Swarm environment
// (swarms/<swarm>/<environment>.yml). Separate files let the owners of the
// configuration repository delegate them to different teams, e.g. with
// CODEOWNERS and branch protection.
const SwarmDir = "swarms"

// SwarmFile is the content of swarms/<swarm>.yml. Its servers belong to
// that Swarm.
type SwarmFile struct {
	Swarm   `yaml:",inline"`
	Servers map[string]Server `yaml:"servers,omitempty"`
}

// EnvironmentFile is the content of swarms/<swarm>/<environment>.yml. Its
// servers belong to that Swarm environment.
type EnvironmentFile struct {
	Environment `yaml:",inline"`
	Servers     map[string]Server `yaml:"servers,omitempty"`
}

func yamlName(name string) (string, bool) {
	for _, ext := range []string{".yml", ".yaml"} {
		if n, ok := strings.CutSuffix(name, ext); ok && n != "" {
			return n, true
		}
	}
	return "", false
}

// loadSwarmDir merges the Swarm and environment files into c. Defining
// a Swarm, environment or server in more than one place is an error, so
// one file cannot override what another one owns.
func (c *Config) loadSwarmDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	if c.Swarms == nil {
		c.Swarms = map[string]Swarm{}
	}
	if c.Servers == nil {
		c.Servers = map[string]Server{}
	}
	origin := map[string]string{} // hostname -> file
	for h := range c.Servers {
		origin[h] = "fleet.yml"
	}
	addServers := func(file, swarm, env string, servers map[string]Server) error {
		for h, srv := range servers {
			if srv.Swarm != "" && srv.Swarm != swarm {
				return fmt.Errorf("%s: server %s cannot be moved to swarm %q from here", file, h, srv.Swarm)
			}
			if env != "" && srv.Environment != "" && srv.Environment != env {
				return fmt.Errorf("%s: server %s cannot be moved to environment %q from here", file, h, srv.Environment)
			}
			if other, dup := origin[h]; dup {
				return fmt.Errorf("%s: server %s is already defined in %s", file, h, other)
			}
			srv.Swarm = swarm
			if env != "" {
				srv.Environment = env
			}
			c.Servers[h] = srv
			origin[h] = file
		}
		return nil
	}

	// Swarm files first, so environment directories can extend them.
	for _, e := range entries {
		name, ok := yamlName(e.Name())
		if e.IsDir() || !ok || strings.HasPrefix(name, ".") {
			continue
		}
		file := filepath.Join(SwarmDir, e.Name())
		if _, dup := c.Swarms[name]; dup {
			return fmt.Errorf("%s: swarm %s is already defined", file, name)
		}
		var sf SwarmFile
		if err := decodeFile(filepath.Join(dir, e.Name()), &sf); err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		c.Swarms[name] = sf.Swarm
		if err := addServers(file, name, "", sf.Servers); err != nil {
			return err
		}
	}

	for _, e := range entries {
		swarm := e.Name()
		if !e.IsDir() || strings.HasPrefix(swarm, ".") {
			continue
		}
		envEntries, err := os.ReadDir(filepath.Join(dir, swarm))
		if err != nil {
			return err
		}
		sort.Slice(envEntries, func(i, j int) bool { return envEntries[i].Name() < envEntries[j].Name() })
		for _, ee := range envEntries {
			env, ok := yamlName(ee.Name())
			if ee.IsDir() || !ok || strings.HasPrefix(env, ".") {
				continue
			}
			file := filepath.Join(SwarmDir, swarm, ee.Name())
			sw := c.Swarms[swarm]
			if _, dup := sw.Environments[env]; dup {
				return fmt.Errorf("%s: environment %s of swarm %s is already defined", file, env, swarm)
			}
			var ef EnvironmentFile
			if err := decodeFile(filepath.Join(dir, swarm, ee.Name()), &ef); err != nil {
				return fmt.Errorf("%s: %w", file, err)
			}
			if sw.Environments == nil {
				sw.Environments = map[string]Environment{}
			}
			sw.Environments[env] = ef.Environment
			c.Swarms[swarm] = sw
			if err := addServers(file, swarm, env, ef.Servers); err != nil {
				return err
			}
		}
	}
	return nil
}

func decodeFile(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return decode(data, v)
}
