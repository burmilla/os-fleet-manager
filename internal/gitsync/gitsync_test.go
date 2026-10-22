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
