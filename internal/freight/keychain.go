package freight

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"

	"juliusbaer.com/codeak/ship/internal/secret"
)

// SecretKeychain reads registry credentials from a secret holding a Docker
// config ({"auths": {"<host>": {...}}}), e.g. a mounted
// kubernetes.io/dockerconfigjson Secret or a vault:// reference. The secret is
// resolved on every lookup, so rotation needs no restart.
//
// An unset secret, or one with no entry for the registry, yields Anonymous so
// a multi-keychain falls through to the next one. Any other error fails closed.
type SecretKeychain struct {
	Source secret.Source
}

// Keychain is the keychain ship uses: the secret first, then Docker's default.
func Keychain(src secret.Source) authn.Keychain {
	return authn.NewMultiKeychain(SecretKeychain{Source: src}, authn.DefaultKeychain)
}

func (k SecretKeychain) Resolve(r authn.Resource) (authn.Authenticator, error) {
	return k.ResolveContext(context.Background(), r)
}

func (k SecretKeychain) ResolveContext(ctx context.Context, r authn.Resource) (authn.Authenticator, error) {
	v, err := k.Source(ctx)
	if secret.IsNotFound(err) {
		return authn.Anonymous, nil
	}
	if err != nil {
		return nil, fmt.Errorf("registry credentials: %w", err)
	}
	var cfg struct {
		Auths map[string]authn.AuthConfig `json:"auths"`
	}
	if err := json.Unmarshal([]byte(v.Reveal()), &cfg); err != nil {
		// Never wrap the JSON error: it can quote the secret.
		return nil, fmt.Errorf("registry credentials: not a Docker config JSON document")
	}
	for key, ac := range cfg.Auths {
		if registryHost(key) == r.RegistryStr() {
			return authn.FromConfig(ac), nil
		}
	}
	return authn.Anonymous, nil
}

// registryHost normalizes a Docker config key ("https://ghcr.io",
// "registry:5000/v2/", "https://index.docker.io/v1/") to a registry host.
func registryHost(key string) string {
	if key == authn.DefaultAuthKey {
		return name.DefaultRegistry
	}
	key = strings.TrimPrefix(strings.TrimPrefix(key, "https://"), "http://")
	host, _, _ := strings.Cut(key, "/")
	return host
}
