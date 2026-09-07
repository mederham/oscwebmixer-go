package config

type Global struct {
	Debug           bool             `json:"debug"`
	OSC             OSCConfig        `json:"osc"`
	Desk            DeskConfig       `json:"desk"`
	ExternalDevices []ExternalDevice `json:"externalDevices"`

	AutoHideChannelPatterns []string `json:"autoHideChannelPatterns"`
	AutoHideGroupPatterns   []string `json:"autoHideGroupPatterns"`
	AutoHideAuxPatterns     []string `json:"autoHideAuxPatterns"`
}

type OSCConfig struct {
	Port int `json:"port"`
}

type DeskConfig struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

type ExternalDevice struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IP        string `json:"ip"`
	Port      int    `json:"port"`
	Broadcast bool   `json:"broadcast"`
	Loopback  bool   `json:"loopback"`
}

const (
	LayoutKindSource  = "source"
	LayoutKindSection = "section"
)

const DefaultLayoutID = "default"

type LayoutItem struct {
	Kind string `json:"kind"`

	Type    string `json:"type"`
	Number  int    `json:"number"`
	Enabled bool   `json:"enabled"`
	Icon    string `json:"icon"`

	SectionID   string `json:"sectionId"`
	SectionName string `json:"sectionName"`
}

type NamedLayout struct {
	Name  string       `json:"name"`
	Items []LayoutItem `json:"items"`
}

type State struct {
	UUID         string                 `json:"uuid"`
	Name         string                 `json:"name"`
	SessionName  string                 `json:"sessionName"`
	Auxes        []Aux                  `json:"auxes"`
	MixerLayouts map[string]NamedLayout `json:"mixerLayouts"`
}

type Aux struct {
	Enabled bool   `json:"enabled"`
	Colour  string `json:"colour"`
	Icon    string `json:"icon"`

	LayoutID string `json:"layoutId"`
}

type Preset struct {
	State
	LastChanged string `json:"lastChanged"`
}
