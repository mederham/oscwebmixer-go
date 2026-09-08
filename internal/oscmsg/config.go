package oscmsg

const (
	LayoutKindSource  = "source"
	LayoutKindSection = "section"
)

const DefaultLayoutID = "default"

type LayoutItem struct {
	Kind string `json:"kind"`

	Type    string `json:"type"`
	Number  int    `json:"number"`
	Label   string `json:"label"`
	Enabled bool   `json:"enabled"`
	Stereo  bool   `json:"stereo"`

	SectionID   string `json:"sectionId"`
	SectionName string `json:"sectionName"`
}

type NamedLayout struct {
	Name  string       `json:"name"`
	Items []LayoutItem `json:"items"`
}

type ConfigPayload struct {
	MixerLayouts map[string]NamedLayout `json:"mixerLayouts"`
	Auxes        []AuxConfig            `json:"auxes"`
	Snapshot     string                 `json:"snapshot"`
}

type AuxConfig struct {
	Channel int    `json:"channel"`
	Label   string `json:"label"`
	Enabled bool   `json:"enabled"`
	Stereo  bool   `json:"stereo"`
	Colour  string `json:"colour"`
	Icon    string `json:"icon"`

	LayoutID string `json:"layoutId"`
}
