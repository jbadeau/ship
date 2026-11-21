// Package builtin resolves driver names to implementations.
package builtin

import (
	"os"

	"github.com/jbadeau/ship/internal/driver"
	"github.com/jbadeau/ship/internal/driver/crossplane"
	"github.com/jbadeau/ship/internal/driver/octopus"
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
		return octopus.Driver{Deps: deps, URL: os.Getenv("OCTOPUS_URL"), APIKey: os.Getenv("OCTOPUS_API_KEY")}, nil
	}
	return driver.LookupExternal(name, deps)
}
