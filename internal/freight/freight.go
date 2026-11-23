// Package freight resolves Freight (immutable OCI artifacts) and their evidence.
//
// ship is artifact agnostic: it only reads manifests/descriptors (digest, media type,
// artifact type, annotations) and never pulls layers. Interpreting content is a
// driver concern.
package freight

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// EvidenceArtifactType is the default artifact type for evidence pushed by ship.
const EvidenceArtifactType = "application/vnd.ship.evidence.v1+json"

// CreatedAnnotation is the standard OCI creation-time annotation (RFC 3339).
// ship stamps it on evidence it pushes; upstreamSoak is measured from it.
const CreatedAnnotation = "org.opencontainers.image.created"

// Descriptor is everything ship knows about an artifact.
type Descriptor struct {
	Ref          string            `json:"ref"`        // registry/repo@sha256:…
	Repository   string            `json:"repository"` // registry/repo
	Digest       string            `json:"digest"`     // sha256:…
	MediaType    string            `json:"mediaType"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Size         int64             `json:"size"`
	Annotations  map[string]string `json:"annotations,omitempty"`
}

// Short returns the first 12 hex characters of the digest.
func (d Descriptor) Short() string { return ShortDigest(d.Digest) }

func ShortDigest(digest string) string {
	h := strings.TrimPrefix(digest, "sha256:")
	if len(h) > 12 {
		h = h[:12]
	}
	return h
}

// Registry is the subset of registry operations ship needs.
type Registry interface {
	// Resolve fetches the descriptor of a digest reference. Tags are rejected.
	Resolve(ctx context.Context, ref string) (Descriptor, error)
	// Referrers lists artifacts whose subject is ref.
	Referrers(ctx context.Context, ref string) ([]Descriptor, error)
	// TagDigest resolves repo:tag to a digest (used by drivers whose targets select by tag).
	TagDigest(ctx context.Context, repository, tag string) (string, error)
	// PushEvidence attaches an evidence artifact to subject and returns its descriptor.
	PushEvidence(ctx context.Context, subject, artifactType string, annotations map[string]string, payload []byte) (Descriptor, error)
}

// OCI implements Registry with go-containerregistry.
type OCI struct {
	insecure map[string]bool
	// Keychain supplies registry credentials. Nil means Docker's default
	// keychain (~/.docker/config.json and credential helpers).
	Keychain authn.Keychain
}

func NewOCI(insecureRegistries []string) *OCI {
	m := map[string]bool{}
	for _, h := range insecureRegistries {
		if h = strings.TrimSpace(h); h != "" {
			m[h] = true
		}
	}
	return &OCI{insecure: m}
}

func (o *OCI) nameOpts(host string) []name.Option {
	opts := []name.Option{name.StrictValidation}
	if o.insecure[host] {
		opts = append(opts, name.Insecure)
	}
	return opts
}

// ParseDigest parses a fully qualified digest reference (no tag, no defaults).
func (o *OCI) ParseDigest(ref string) (name.Digest, error) {
	d, err := name.NewDigest(ref, name.StrictValidation)
	if err != nil {
		return name.Digest{}, fmt.Errorf("%q is not a fully qualified digest reference (registry/repo@sha256:…): %w", ref, err)
	}
	return name.NewDigest(ref, o.nameOpts(d.RegistryStr())...)
}

func (o *OCI) repo(repository string) (name.Repository, error) {
	r, err := name.NewRepository(repository, name.StrictValidation)
	if err != nil {
		return name.Repository{}, err
	}
	return name.NewRepository(repository, o.nameOpts(r.RegistryStr())...)
}

func (o *OCI) remoteOpts(ctx context.Context) []remote.Option {
	kc := o.Keychain
	if kc == nil {
		kc = authn.DefaultKeychain
	}
	return []remote.Option{remote.WithContext(ctx), remote.WithAuthFromKeychain(kc)}
}

// manifestMeta is the minimal, type-agnostic view of a manifest or index.
type manifestMeta struct {
	MediaType    string                      `json:"mediaType"`
	ArtifactType string                      `json:"artifactType"`
	Config       *struct{ MediaType string } `json:"config"`
	Annotations  map[string]string           `json:"annotations"`
}

func (o *OCI) Resolve(ctx context.Context, ref string) (Descriptor, error) {
	d, err := o.ParseDigest(ref)
	if err != nil {
		return Descriptor{}, err
	}
	rd, err := remote.Get(d, o.remoteOpts(ctx)...)
	if err != nil {
		return Descriptor{}, fmt.Errorf("resolve %s: %w", ref, err)
	}
	if rd.Digest.String() != d.DigestStr() {
		return Descriptor{}, fmt.Errorf("resolve %s: registry returned digest %s", ref, rd.Digest)
	}
	var m manifestMeta
	if err := json.Unmarshal(rd.Manifest, &m); err != nil {
		return Descriptor{}, fmt.Errorf("resolve %s: parse manifest: %w", ref, err)
	}
	at := m.ArtifactType
	if at == "" && m.Config != nil {
		at = m.Config.MediaType // OCI 1.1: config media type is the fallback artifact type
	}
	return Descriptor{
		Ref:          d.Context().Name() + "@" + d.DigestStr(),
		Repository:   d.Context().Name(),
		Digest:       d.DigestStr(),
		MediaType:    string(rd.MediaType),
		ArtifactType: at,
		Size:         rd.Size,
		Annotations:  m.Annotations,
	}, nil
}

func (o *OCI) Referrers(ctx context.Context, ref string) ([]Descriptor, error) {
	d, err := o.ParseDigest(ref)
	if err != nil {
		return nil, err
	}
	idx, err := remote.Referrers(d, o.remoteOpts(ctx)...)
	if err != nil {
		return nil, fmt.Errorf("referrers of %s: %w", ref, err)
	}
	im, err := idx.IndexManifest()
	if err != nil {
		return nil, err
	}
	out := make([]Descriptor, 0, len(im.Manifests))
	for _, m := range im.Manifests {
		out = append(out, Descriptor{
			Ref:          d.Context().Name() + "@" + m.Digest.String(),
			Repository:   d.Context().Name(),
			Digest:       m.Digest.String(),
			MediaType:    string(m.MediaType),
			ArtifactType: m.ArtifactType,
			Size:         m.Size,
			Annotations:  m.Annotations,
		})
	}
	return out, nil
}

func (o *OCI) TagDigest(ctx context.Context, repository, tag string) (string, error) {
	r, err := o.repo(repository)
	if err != nil {
		return "", err
	}
	desc, err := remote.Head(r.Tag(tag), o.remoteOpts(ctx)...)
	if err != nil {
		return "", fmt.Errorf("resolve %s:%s: %w", repository, tag, err)
	}
	return desc.Digest.String(), nil
}

func (o *OCI) PushEvidence(ctx context.Context, subject, artifactType string, annotations map[string]string, payload []byte) (Descriptor, error) {
	d, err := o.ParseDigest(subject)
	if err != nil {
		return Descriptor{}, err
	}
	sd, err := remote.Head(d, o.remoteOpts(ctx)...)
	if err != nil {
		return Descriptor{}, fmt.Errorf("evidence subject %s: %w", subject, err)
	}
	img := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	// The config media type carries the artifact type, which every OCI 1.1 registry
	// (and the referrers tag-schema fallback) reports as artifactType.
	img = mutate.ConfigMediaType(img, types.MediaType(artifactType))
	if len(payload) > 0 {
		img, err = mutate.AppendLayers(img, static.NewLayer(payload, "application/json"))
		if err != nil {
			return Descriptor{}, err
		}
	}
	if _, ok := annotations[CreatedAnnotation]; !ok {
		withTime := make(map[string]string, len(annotations)+1)
		for k, v := range annotations {
			withTime[k] = v
		}
		withTime[CreatedAnnotation] = time.Now().UTC().Format(time.RFC3339)
		annotations = withTime
	}
	img = mutate.Annotations(img, annotations).(v1.Image)
	img = mutate.Subject(img, v1.Descriptor{MediaType: sd.MediaType, Digest: sd.Digest, Size: sd.Size}).(v1.Image)
	h, err := img.Digest()
	if err != nil {
		return Descriptor{}, err
	}
	target := d.Context().Digest(h.String())
	if err := remote.Write(target, img, o.remoteOpts(ctx)...); err != nil {
		return Descriptor{}, fmt.Errorf("push evidence for %s: %w", subject, err)
	}
	return Descriptor{
		Ref: d.Context().Name() + "@" + h.String(), Repository: d.Context().Name(), Digest: h.String(),
		MediaType: string(types.OCIManifestSchema1), ArtifactType: artifactType, Annotations: annotations,
	}, nil
}

// Matches reports whether a referrer satisfies an evidence requirement.
func Matches(ref Descriptor, artifactType string, annotations map[string]string) bool {
	if ref.ArtifactType != artifactType {
		return false
	}
	for k, v := range annotations {
		if ref.Annotations[k] != v {
			return false
		}
	}
	return true
}
