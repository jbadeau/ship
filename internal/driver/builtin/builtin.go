// Package builtin resolves driver names to implementations.
package builtin

import (
	"os"

	"juliusbaer.com/codeak/ship/internal/driver"
	"juliusbaer.com/codeak/ship/internal/driver/crossplane"
	"juliusbaer.com/codeak/ship/internal/driver/octopus"
	"juliusbaer.com/codeak/ship/internal/secret"
)

// Get returns a built-in driver, or ship-driver-<name> from PATH.
func Get(name string, deps driver.Deps) (driver.Driver, error) {
	switch name {
	case "noop":
		return driver.Noop{Deps: deps}, nil
	case "exec":
		return driver.Exec{Deps: deps}, nil
	case "crossplane":
		return crossplane.Driver{Deps: deps}, nil
	case "octopus":
		d := octopus.Driver{Deps: deps, URL: os.Getenv("OCTOPUS_URL")}
		if deps.Secrets != nil {
			d.APIKey = deps.Secrets.Source(secret.OctopusAPIKey)
		}
		return d, nil
	}
	return driver.LookupExternal(name, deps)
}
