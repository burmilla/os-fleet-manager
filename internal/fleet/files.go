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
// CODEOWNERS and branch protection. A file can only affect servers which
// are members of its Swarm or environment.
const SwarmDir = "swarms"

func yamlName(name string) (string, bool) {
	for _, ext := range []string{".yml", ".yaml"} {
		if n, ok := strings.CutSuffix(name, ext); ok && n != "" {
			return n, true
		}
	}
	return "", false
}

// loadSwarmDir merges the Swarm and environment files into c. Defining a
// Swarm or environment in more than one place is an error, so one file
// cannot override what another one owns.
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
		var sw Swarm
		if err := decodeFile(filepath.Join(dir, e.Name()), &sw); err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		c.Swarms[name] = sw
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
			var e Environment
			if err := decodeFile(filepath.Join(dir, swarm, ee.Name()), &e); err != nil {
				return fmt.Errorf("%s: %w", file, err)
			}
			if sw.Environments == nil {
				sw.Environments = map[string]Environment{}
			}
			sw.Environments[env] = e
			c.Swarms[swarm] = sw
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
