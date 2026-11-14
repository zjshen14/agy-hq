package templates

import "embed"

// FS embeds the default Vault bootstrap templates and HQ Console sidecar scripts
// so a standalone ~/.local/bin/hq binary can initialize and migrate Vaults anywhere.
//
//go:embed USER.md MEMORY.md config.yaml triggers.yaml gitignore.tmpl sidecars
var FS embed.FS
