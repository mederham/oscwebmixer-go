// Package config loads/saves the JSON files under ./config/: global.json
// (server/OSC/desk network settings, external devices), state.json (the
// live admin-editable source/aux overrides — see plan: "Group buses" for
// why sources is a single ordered, typed list rather than the old
// channels[]/auxes[] split), and presets.json (named saved states).
package config

// Global mirrors config/global.json.
type Global struct {
	Debug           bool             `json:"debug"`
	Server          ServerConfig     `json:"server"`
	OSC             OSCConfig        `json:"osc"`
	Desk            DeskConfig       `json:"desk"`
	ExternalDevices []ExternalDevice `json:"externalDevices"`
}

type ServerConfig struct {
	Port int `json:"port"`
}

type OSCConfig struct {
	Port int `json:"port"`
}

type DeskConfig struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

// ExternalDevice is one relay target — TouchOSC, Reaper, StreamDeck, or
// similar — see the plan's external-device relay section. Broadcast gates
// whether it receives fan-out at all; Loopback controls whether it also
// receives messages that originated from itself (not yet meaningfully
// exercised — see hub's broadcastSource doc comment).
type ExternalDevice struct {
	Name      string `json:"name"`
	IP        string `json:"ip"`
	Port      int    `json:"port"`
	Broadcast bool   `json:"broadcast"`
	Loopback  bool   `json:"loopback"`
}

// State mirrors config/state.json — the admin-editable overlay merged onto
// live desk data by hub.buildConfig(). Sources is a single ordered,
// type-tagged list (not separate channels[]/auxes[] arrays) so a group can
// sit between two channels in display order — see plan: "Group buses".
type State struct {
	UUID        string   `json:"uuid"`
	Name        string   `json:"name"`
	SessionName string   `json:"sessionName"`
	Auxes       []Aux    `json:"auxes"`
	Sources     []Source `json:"sources"`
}

// Aux is the admin-editable overlay for one aux bus, indexed 1:1 by
// position (aux N is Auxes[N-1]) since auxes stay the fixed destination
// concept — only Sources needs identity-by-value (type+number) since it's
// reorderable/interleavable.
type Aux struct {
	Enabled bool   `json:"enabled"`
	Colour  string `json:"colour"`
	Icon    string `json:"icon"`
}

// Source is the admin-editable overlay for one mixable source (a channel
// or a group bus — see plan). Type+Number is its identity; DisplayOrder is
// an explicit field (not array position) so drag-and-drop reordering in
// the admin UI is a plain field update.
type Source struct {
	Type         string `json:"type"` // "channel" | "group"
	Number       int    `json:"number"`
	Enabled      bool   `json:"enabled"`
	DisplayOrder int    `json:"displayOrder"`
	Title        string `json:"title"`
	Icon         string `json:"icon"`
}

// Preset is a named, saved snapshot of State — mirrors JS's
// {...structuredClone(currentState), uuid, name, lastChanged} shape via
// embedding: State's own UUID/Name fields serve double duty as the
// preset's identity, matching the JS spread-and-override pattern exactly.
type Preset struct {
	State
	LastChanged string `json:"lastChanged"`
}
