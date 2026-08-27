package oscmsg

// LayoutKindSource/LayoutKindSection are LayoutItem.Kind's two values —
// mirrors config.LayoutKindSource/LayoutKindSection, kept as separate
// constants since this is the wire-facing type, not the persisted one.
const (
	LayoutKindSource  = "source"
	LayoutKindSection = "section"
)

// LayoutItem is one entry in ConfigPayload.MixerLayout — the single
// ordered sequence a mixer/admin client renders and drags, mixing sources
// (channels and groups — see the plan's "Group buses" section, both sent
// to auxes via the same Aux_Send/N/send_level|send_pan address shape,
// just under different namespace roots) and admin-created section
// markers. See config.LayoutItem's doc comment for why array position —
// not a stored order/section field — is the single source of truth for
// both display order and a source's section membership.
type LayoutItem struct {
	Kind string `json:"kind"` // LayoutKindSource | LayoutKindSection

	// Set when Kind == LayoutKindSource.
	Type    string `json:"type"` // "channel" | "group"
	Number  int    `json:"number"`
	Label   string `json:"label"`
	Enabled bool   `json:"enabled"`
	Stereo  bool   `json:"stereo"`
	Icon    string `json:"icon"`

	// Set when Kind == LayoutKindSection.
	SectionID   string `json:"sectionId"`
	SectionName string `json:"sectionName"`
}

// ConfigPayload is what a mixer client needs to render itself: the
// interleaved mixer layout (see LayoutItem) and the list of destination
// aux buses.
type ConfigPayload struct {
	MixerLayout []LayoutItem `json:"mixerLayout"`
	Auxes       []AuxConfig  `json:"auxes"`
	Snapshot    string       `json:"snapshot"`
}

// AuxConfig describes one destination aux bus.
type AuxConfig struct {
	Channel int    `json:"channel"`
	Label   string `json:"label"`
	Enabled bool   `json:"enabled"`
	Stereo  bool   `json:"stereo"`
	Colour  string `json:"colour"`
	Icon    string `json:"icon"`
}
