// Package specs embeds the official vendor API specifications (see SOURCES.md).
package specs

import _ "embed"

//go:embed kosli-openapi.json
var Kosli []byte

//go:embed octopus-swagger.json
var Octopus []byte
