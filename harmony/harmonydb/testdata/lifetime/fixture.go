// Package lifetime embeds the exact historical migration from 3631dd287043.
// It is test-only data, never part of the production migration embed.
package lifetime

import "embed"

//go:embed sql/*.sql
var SQL embed.FS
