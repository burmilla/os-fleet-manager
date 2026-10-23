package gitsync

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestSync(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	src := t.TempDir()
	run(t, src, "init", "--quiet", "--initial-branch=main")
	write := func(content string) {
		if err := os.WriteFile(filepath.Join(src, "fleet.yml"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		run(t, src, "add", ".")
		run(t, src, "commit", "--quiet", "-m", content)
	}
	write("one")

	repo := &Repo{URL: "file://" + src, Branch: "main", Token: "secret", Dir: filepath.Join(t.TempDir(), "repo")}
	rev1, err := repo.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	write("two")
	rev2, err := repo.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rev1 == rev2 {
		t.Fatal("revision should change")
	}
	data, _ := os.ReadFile(filepath.Join(repo.Dir, "fleet.yml"))
	if string(data) != "two" {
		t.Fatalf("checkout not updated: %q", data)
	}
	cfg, _ := os.ReadFile(filepath.Join(repo.Dir, ".git", "config"))
	if strings.Contains(string(cfg), "secret") {
		t.Fatal("token leaked to .git/config")
	}
}

func TestSyncPath(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	src := t.TempDir()
	run(t, src, "init", "--quiet", "--initial-branch=main")
	// file:// does not use partial clone unless allowed; it still works.
	run(t, src, "config", "uploadpack.allowFilter", "true")
	for name, content := range map[string]string{
		"README.md":                 "app",
		"src/main.go":               "package main",
		"ops/fleet/fleet.yml":       "one",
		"ops/fleet/swarms/a.yml":    "a",
		"ops/fleet-other/fleet.yml": "other",
	} {
		p := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run(t, src, "add", ".")
	run(t, src, "commit", "--quiet", "-m", "init")

	repo := &Repo{URL: "file://" + src, Branch: "main", Dir: filepath.Join(t.TempDir(), "repo"), Path: "ops/fleet"}
	if _, err := repo.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	check := func(want string) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(repo.ConfigDir(), "fleet.yml"))
		if err != nil || string(data) != want {
			t.Fatalf("fleet.yml: %q %v", data, err)
		}
		if _, err := os.Stat(filepath.Join(repo.ConfigDir(), "swarms", "a.yml")); err != nil {
			t.Fatal(err)
		}
		for _, other := range []string{"src/main.go", "ops/fleet-other/fleet.yml"} {
			if _, err := os.Stat(filepath.Join(repo.Dir, other)); err == nil {
				t.Fatalf("%s should not be checked out", other)
			}
		}
	}
	check("one")

	if err := os.WriteFile(filepath.Join(src, "ops/fleet/fleet.yml"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, src, "commit", "--quiet", "-am", "two")
	if _, err := repo.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	check("two")

	for _, bad := range []string{"../x", "/etc", ".git", ".git/hooks"} {
		if ValidPath(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
	for _, good := range []string{"", "ops/fleet", ".github/fleet", "fleet/"} {
		if !ValidPath(good) {
			t.Errorf("%q should be valid", good)
		}
	}
}

// A checkout made by an earlier version (full clone) keeps working.
func TestSyncExistingFullClone(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	src := t.TempDir()
	run(t, src, "init", "--quiet", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(src, "fleet.yml"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, src, "add", ".")
	run(t, src, "commit", "--quiet", "-m", "one")
	dir := filepath.Join(t.TempDir(), "repo")
	run(t, "", "clone", "--quiet", "--depth", "1", "--branch", "main", "file://"+src, dir)

	repo := &Repo{URL: "file://" + src, Branch: "main", Dir: dir}
	if _, err := repo.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
}
