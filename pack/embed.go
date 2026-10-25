// Package pack embeds the shipped, model-keyed persona pack and behavior
// profiles so a fresh binary works without an external configuration dir.
package pack

import "embed"

//go:embed all:agents
var FS embed.FS
