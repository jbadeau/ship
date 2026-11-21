// Package gitx is a thin wrapper around the git CLI.
package gitx

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

type Repo struct{ Dir string }

// Run executes git and returns trimmed stdout.
func (r Repo) Run(args ...string) (string, error) {
	out, err := r.raw(nil, args...)
	return strings.TrimSpace(string(out)), err
}

func (r Repo) raw(stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// Show returns the content of path at rev. found is false if the path does not exist there.
func (r Repo) Show(rev, path string) (content []byte, found bool, err error) {
	if _, err := r.raw(nil, "cat-file", "-e", rev+":"+path); err != nil {
		if _, verr := r.raw(nil, "rev-parse", "--verify", "--quiet", rev+"^{commit}"); verr != nil {
			return nil, false, fmt.Errorf("unknown revision %q", rev)
		}
		return nil, false, nil
	}
	b, err := r.raw(nil, "show", rev+":"+path)
	return b, err == nil, err
}

// Resolve returns the full commit sha of rev.
func (r Repo) Resolve(rev string) (string, error) {
	return r.Run("rev-parse", "--verify", rev+"^{commit}")
}
