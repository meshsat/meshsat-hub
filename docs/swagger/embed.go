// Package swagger provides the generated OpenAPI spec for embedding.
package swagger

import "embed"

// SpecFS embeds the generated swagger.json and swagger.yaml files.
//
//go:embed swagger.json swagger.yaml
var SpecFS embed.FS
