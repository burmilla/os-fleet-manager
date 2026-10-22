// Package gitsync keeps a local checkout of the fleet configuration
// repository up to date. It shells out to the git CLI which works with
// both GitHub and Azure DevOps using a personal access token.
package gitsync

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Repo struct {
	URL      string
	Branch   string
	Username string
	Token    string
	Dir      string
}

func (r *Repo) env() []string {
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if r.Token != "" {
		user := r.Username
		if user == "" {
			user = "git"
		}
		// Pass the credential as a header through environment so that it
		// is never written to .git/config or visible in the process list.
		auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + r.Token))
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader",
			"GIT_CONFIG_VALUE_0=Authorization: Basic "+auth,
		)
	}
	return env
}

func (r *Repo) git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = r.env()
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(out.String())
		if r.Token != "" {
			msg = strings.ReplaceAll(msg, r.Token, "***")
		}
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// Sync clones the repository on first call and fetches the branch on later
// calls. It returns the commit hash of the checked out revision.
func (r *Repo) Sync(ctx context.Context) (string, error) {
	branch := r.Branch
	if branch == "" {
		branch = "main"
	}
	if _, err := os.Stat(filepath.Join(r.Dir, ".git")); err != nil {
		if err := os.MkdirAll(filepath.Dir(r.Dir), 0o755); err != nil {
			return "", err
		}
		_ = os.RemoveAll(r.Dir)
		if _, err := r.git(ctx, "", "clone", "--quiet", "--depth", "1", "--branch", branch, r.URL, r.Dir); err != nil {
			return "", err
		}
	} else {
		if _, err := r.git(ctx, r.Dir, "fetch", "--quiet", "--depth", "1", r.URL, branch); err != nil {
			return "", err
		}
		if _, err := r.git(ctx, r.Dir, "reset", "--quiet", "--hard", "FETCH_HEAD"); err != nil {
			return "", err
		}
	}
	return r.git(ctx, r.Dir, "rev-parse", "HEAD")
}
