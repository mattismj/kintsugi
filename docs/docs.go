// Package docs embeds the shipped man pages and command reference so a single
// installed binary can answer `kintsugi docs <command>` without the repository
// being present. The files are the same ones the repo carries — embedding is the
// delivery mechanism, not a second copy to keep in sync.
package docs

import "embed"

// Man holds docs/man/*.1, the roff man pages.
//
//go:embed man
var Man embed.FS

// Reference holds docs/reference/*.md, the Markdown command reference.
//
//go:embed reference
var Reference embed.FS
