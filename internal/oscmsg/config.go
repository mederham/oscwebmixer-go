package oscmsg

// LayoutKindSource/LayoutKindSection are LayoutItem.Kind's two values —
// mirrors config.LayoutKindSource/LayoutKindSection, kept as separate
// constants since this is the wire-facing type, not the persisted one.
const (
	LayoutKindSource  = "source"
	LayoutKindSection = "section"
)

// DefaultLayoutID mirrors config.DefaultLayoutID — see its doc comment.
const DefaultLayoutID = "default"

// LayoutItem is one entry in a NamedLayout's Items — the single ordered
// sequence a mixer/admin client renders and drags, mixing sources
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

// NamedLayout is one entry in ConfigPayload.MixerLayouts — mirrors
// config.NamedLayout, duplicated here since this is the wire-facing
// shape, not the persisted one (same reasoning as LayoutItem/LayoutKind
// above). Name rides along even though the mixer client only ever looks a
// layout up by its map key and ignores it: ConfigPayload doubles as
// internal/admin's AdminQuery reply too (see hub.DeskStatusInfo's doc
// comment for the same "one payload, two consumers" shape), and the admin
// UI's layout picker/preset save both need the name without a second
// fetch path back to state.json.
type NamedLayout struct {
	Name  string       `json:"name"`
	Items []LayoutItem `json:"items"`
}

// ConfigPayload is what a mixer client needs to render itself: every
// named mixer layout (see NamedLayout — the client picks one by whichever
// aux is selected, see AuxConfig.LayoutID) and the list of destination
// aux buses.
type ConfigPayload struct {
	MixerLayouts map[string]NamedLayout `json:"mixerLayouts"`
	Auxes        []AuxConfig             `json:"auxes"`
	Snapshot     string                  `json:"snapshot"`
}

// AuxConfig describes one destination aux bus.
type AuxConfig struct {
	Channel int    `json:"channel"`
	Label   string `json:"label"`
	Enabled bool   `json:"enabled"`
	Stereo  bool   `json:"stereo"`
	Colour  string `json:"colour"`
	Icon    string `json:"icon"`

	// LayoutID names which entry in ConfigPayload.MixerLayouts this aux
	// renders — mirrors config.Aux.LayoutID's doc comment. Empty (or
	// naming an id no longer in MixerLayouts) means the mixer client
	// falls back to DefaultLayoutID — see mixer.js's layoutFor().
	LayoutID string `json:"layoutId"`
}
