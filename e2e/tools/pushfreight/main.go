// pushfreight publishes a test Freight (a small OCI artifact) the way a build
// pipeline would: tagged with its version and annotated with
// org.opencontainers.image.version. Prints registry/repo@sha256:….
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func main() {
	repo := flag.String("repo", "", "registry/repository")
	version := flag.String("version", "", "version (also the tag)")
	at := flag.String("artifact-type", "application/vnd.example.deploy-bundle.v1", "artifact type")
	flag.Parse()
	if *repo == "" || *version == "" {
		flag.Usage()
		os.Exit(2)
	}
	content := fmt.Sprintf(`{"version":%q,"built":%q}`, *version, time.Now().UTC().Format(time.RFC3339Nano))
	img := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, types.MediaType(*at))
	img, err := mutate.AppendLayers(img, static.NewLayer([]byte(content), "application/json"))
	check(err)
	img = mutate.Annotations(img, map[string]string{"org.opencontainers.image.version": *version}).(v1.Image)
	tag, err := name.NewTag(*repo+":"+*version, name.Insecure)
	check(err)
	check(remote.Write(tag, img))
	d, err := img.Digest()
	check(err)
	fmt.Println(*repo + "@" + d.String())
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
