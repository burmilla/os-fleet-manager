// Package gitsync keeps a local checkout of the fleet configuration
// repository up to date. It shells out to the git CLI which works with
// both GitHub and Azure DevOps using a personal access token.
package gitsync

import (
	"bytes"
	"context"
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
	// Path limits the checkout to one directory of the repository, so
	// the configuration can live in a repository used for other things
	// too. Empty means the whole repository.
	Path string
}

// ValidPath reports whether p can be used as Repo.Path: a relative path
// inside the repository.
func ValidPath(p string) bool {
	if p == "" {
		return true
	}
	first, _, _ := strings.Cut(filepath.ToSlash(filepath.Clean(p)), "/")
	return filepath.IsLocal(p) && first != ".git"
}

// credentialHelper answers git's credential requests from the
// environment of the git process.
const credentialHelper = `!f() { test "$1" = get && printf 'username=%s\npassword=%s\n' "$FLEET_GIT_CREDENTIAL_USERNAME" "$FLEET_GIT_CREDENTIAL_TOKEN"; }; f`

func (r *Repo) env() []string {
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if r.Token != "" {
		user := r.Username
		if user == "" {
			user = "git"
		}
		// Pass the credential through a credential helper reading the
		// environment so that it is never written to .git/config or
		// visible in the process list. Unlike an Authorization header it
		// is also sent after a redirect to another host (e.g. Azure DevOps
		// visualstudio.com to dev.azure.com), and git reports a rejected
		// token as an authentication failure.
		env = append(env,
			"FLEET_GIT_CREDENTIAL_USERNAME="+user,
			"FLEET_GIT_CREDENTIAL_TOKEN="+r.Token,
			"GIT_CONFIG_COUNT=2",
			// An empty value drops helpers configured elsewhere.
			"GIT_CONFIG_KEY_0=credential.helper",
			"GIT_CONFIG_VALUE_0=",
			"GIT_CONFIG_KEY_1=credential.helper",
			"GIT_CONFIG_VALUE_1="+credentialHelper,
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
		if r.Token == "" && strings.Contains(msg, "could not read Username") {
			msg += " (the repository needs a token: set FLEET_GIT_TOKEN or FLEET_GIT_TOKEN_FILE)"
		}
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// Sync clones the repository on first call and fetches the branch on later
// calls. It returns the commit hash of the checked out revision.
//
// With Path set only that directory is checked out (sparse checkout), and
// file contents are only downloaded for it where the server supports
// partial clone (GitHub and Azure DevOps do; others send everything).
func (r *Repo) Sync(ctx context.Context) (string, error) {
	branch := r.Branch
	if branch == "" {
		branch = "main"
	}
	if !ValidPath(r.Path) {
		return "", fmt.Errorf("invalid repository path %q", r.Path)
	}
	target := "FETCH_HEAD"
	if _, err := os.Stat(filepath.Join(r.Dir, ".git")); err != nil {
		target = "HEAD"
		if err := os.MkdirAll(filepath.Dir(r.Dir), 0o755); err != nil {
			return "", err
		}
		_ = os.RemoveAll(r.Dir)
		if _, err := r.git(ctx, "", "clone", "--quiet", "--depth", "1", "--filter=blob:none", "--no-checkout",
			"--branch", branch, r.URL, r.Dir); err != nil {
			return "", err
		}
	} else {
		// origin is also where missing file contents are fetched from.
		if _, err := r.git(ctx, r.Dir, "remote", "set-url", "origin", r.URL); err != nil {
			return "", err
		}
		fetch := []string{"fetch", "--quiet", "--depth", "1", "origin", branch}
		// Checkouts made by earlier versions are not partial clones.
		if partial, _ := r.git(ctx, r.Dir, "config", "--get", "remote.origin.promisor"); partial == "true" {
			fetch = append(fetch, "--filter=blob:none")
		}
		if _, err := r.git(ctx, r.Dir, fetch...); err != nil {
			return "", err
		}
	}
	sparse := []string{"sparse-checkout", "disable"}
	if r.Path != "" {
		sparse = []string{"sparse-checkout", "set", "--", filepath.ToSlash(filepath.Clean(r.Path))}
	}
	if _, err := r.git(ctx, r.Dir, sparse...); err != nil {
		return "", err
	}
	if _, err := r.git(ctx, r.Dir, "reset", "--quiet", "--hard", target); err != nil {
		return "", err
	}
	return r.git(ctx, r.Dir, "rev-parse", "HEAD")
}

// ConfigDir is the directory of the checkout which holds the configuration.
func (r *Repo) ConfigDir() string {
	return filepath.Join(r.Dir, r.Path)
}
