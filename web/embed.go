// Package web embeds the admin dashboard and developer hub in the RelayOps product binary.
package web

import "embed"

//go:embed static
var Static embed.FS
