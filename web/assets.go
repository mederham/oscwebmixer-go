// Package web embeds the static frontend assets into the binary — chosen
// specifically because the deploy target is a container on a constrained
// ARM router (see plan: "Asset packaging — embed-only"): single
// self-contained artifact, nothing else to manage on limited on-device
// storage.
package web

import "embed"

// MixerFS holds the mixer page (the live show-time UI). Rooted at "mixer"
// so callers strip that prefix via fs.Sub before serving.
//
//go:embed mixer
var MixerFS embed.FS

// AdminFS holds the admin panel's templates and static assets (admin.css).
// Rooted at "admin" so callers strip that prefix via fs.Sub before serving.
//
//go:embed admin
var AdminFS embed.FS
