package templates

import "embed"

// FS embeds the default Vault bootstrap templates, HQ Console sidecar scripts,
// and reference connector manifests so a standalone hq binary works anywhere.
//
//go:embed USER.md MEMORY.md config.yaml triggers.yaml gitignore.tmpl sidecars connectors skills
var FS embed.FS
