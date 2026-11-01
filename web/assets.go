package web

import "embed"

//go:embed mixer
var MixerFS embed.FS

//go:embed admin
var AdminFS embed.FS

//go:embed assets
var AssetsFS embed.FS
