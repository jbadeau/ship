// Package testutil provides an in-memory OCI registry and scratch git repos for tests.
package testutil

import (
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"juliusbaer.com/codeak/ship/internal/gitx"
)

// Registry starts an in-memory OCI registry and returns its host:port.
func Registry(t *testing.T) string {
	t.Helper()
	s := httptest.NewServer(registry.New(registry.WithReferrersSupport(true), registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(s.Close)
	return strings.TrimPrefix(s.URL, "http://")
}

// PushArtifact pushes a random artifact of the given type to repo:tag and returns repo@digest.
func PushArtifact(t *testing.T, repo, tag, artifactType string, annotations map[string]string) string {
	t.Helper()
	img, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	img = mutate.MediaType(img, types.OCIManifestSchema1)
	if artifactType != "" {
		img = mutate.ConfigMediaType(img, types.MediaType(artifactType))
	}
	if annotations != nil {
		img = mutate.Annotations(img, annotations).(v1.Image)
	}
	ref, err := name.NewTag(repo+":"+tag, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	d, _ := img.Digest()
	return repo + "@" + d.String()
}

// GitRepo creates a scratch repo on branch main.
func GitRepo(t *testing.T) gitx.Repo {
	t.Helper()
	r := gitx.Repo{Dir: t.TempDir()}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
		{"config", "commit.gpgsign", "false"},
	} {
		Must(t, r, args...)
	}
	return r
}

func Must(t *testing.T, r gitx.Repo, args ...string) string {
	t.Helper()
	out, err := r.Run(args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Commit writes files (path -> content; "" deletes) and commits them, returning the sha.
func Commit(t *testing.T, r gitx.Repo, msg string, files map[string]string) string {
	t.Helper()
	for p, c := range files {
		abs := filepath.Join(r.Dir, p)
		if c == "" {
			Must(t, r, "rm", "-q", p)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
		Must(t, r, "add", p)
	}
	Must(t, r, "commit", "-q", "--allow-empty", "-m", msg)
	return Must(t, r, "rev-parse", "HEAD")
}
