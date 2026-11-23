// Command fakes serves the Octopus and Kosli fakes for the e2e sandbox. Every
// request and response is validated against the official vendor specs; the
// recorded violations are at GET /_contract/violations on each port.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"juliusbaer.com/codeak/ship/internal/fakes/kosli"
	"juliusbaer.com/codeak/ship/internal/fakes/octopus"
)

type octopusSeed struct {
	FailEnvironments []string `yaml:"failEnvironments"`
	Spaces           map[string]struct {
		Projects     []octopus.Project     `yaml:"projects"`
		Environments []octopus.Environment `yaml:"environments"`
	} `yaml:"spaces"`
}

func main() {
	octAddr := flag.String("octopus", ":8081", "Octopus fake listen address")
	octSeed := flag.String("octopus-seed", "", "YAML with spaces/projects/environments")
	kosliAddr := flag.String("kosli", ":8082", "Kosli fake listen address")
	kosliOrg := flag.String("kosli-org", "bank", "Kosli organization")
	flag.Parse()

	of := octopus.New(os.Getenv("OCTOPUS_API_KEY"))
	if *octSeed != "" {
		b, err := os.ReadFile(*octSeed)
		if err != nil {
			log.Fatal(err)
		}
		var s octopusSeed
		if err := yaml.Unmarshal(b, &s); err != nil {
			log.Fatal(err)
		}
		of.FailEnvironments = s.FailEnvironments
		for space, sp := range s.Spaces {
			for _, p := range sp.Projects {
				of.AddProject(space, p)
			}
			for _, e := range sp.Environments {
				of.AddEnvironment(space, e)
			}
		}
	}
	og, err := of.Server()
	if err != nil {
		log.Fatal(err)
	}

	// KOSLI_TOKENS: comma-separated token=identity pairs.
	tokens := map[string]string{}
	for _, kv := range strings.Split(os.Getenv("KOSLI_TOKENS"), ",") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			tokens[k] = v
		}
	}
	kg, err := kosli.New(*kosliOrg, tokens).Server()
	if err != nil {
		log.Fatal(err)
	}

	go func() { log.Fatal(http.ListenAndServe(*kosliAddr, logged("kosli", kg))) }()
	log.Printf("octopus fake on %s, kosli fake on %s (org %s)", *octAddr, *kosliAddr, *kosliOrg)
	log.Fatal(http.ListenAndServe(*octAddr, logged("octopus", og)))
}

func logged(name string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s %s", name, r.Method, r.URL.RequestURI())
		h.ServeHTTP(w, r)
	})
}
