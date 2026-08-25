package oscmsg

// ConfigPayload is what a mixer client needs to render itself: the ordered,
// interleavable list of mixable sources (channels and groups — see the
// plan's "Group buses" section) and the list of destination aux buses.
type ConfigPayload struct {
	Sources  []SourceConfig `json:"sources"`
	Auxes    []AuxConfig    `json:"auxes"`
	Snapshot string         `json:"snapshot"`
}

// SourceConfig describes one mixable source — an input channel or a group
// bus output, both sent to auxes via the same Aux_Send/N/send_level|
// send_pan address shape (just under different namespace roots:
// Input_Channels vs Group_Outputs).
type SourceConfig struct {
	Type    string `json:"type"` // "channel" | "group"
	Number  int    `json:"number"`
	Label   string `json:"label"`
	Enabled bool   `json:"enabled"`
	Stereo  bool   `json:"stereo"`
	Order   int    `json:"order"`
	Title   string `json:"title"`
	Icon    string `json:"icon"`
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
