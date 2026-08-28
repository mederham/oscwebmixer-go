// Package config loads/saves the JSON files under ./config/: global.json
// (server/OSC/desk network settings, external devices), state.json (the
// live admin-editable mixer layout/aux overrides — see LayoutItem for why
// sources and admin-created sections share one ordered list rather than
// separate channels[]/auxes[] arrays or a stored per-source section field),
// and presets.json (named saved states).
package config

// Global mirrors config/global.json. The HTTP server port deliberately
// isn't a field here — see OSCWEBMIXER_HTTP_PORT in main.go: it's not
// admin-editable (rebinding the listener serving the very request that
// changed it isn't something to support) and round-tripping it through
// this file anyway would just mean a stale value silently wins on every
// startup where the env var isn't set — env var-or-hardcoded-default is
// the only source of truth, deliberately never persisted.
type Global struct {
	Debug           bool             `json:"debug"`
	OSC             OSCConfig        `json:"osc"`
	Desk            DeskConfig       `json:"desk"`
	ExternalDevices []ExternalDevice `json:"externalDevices"`

	// AutoHideChannelPatterns/AutoHideGroupPatterns/AutoHideAuxPatterns
	// each list case-insensitive substrings matched against that type's
	// live desk-reported name only — kept separate per type (not one
	// shared list) because names collide by convention across types: an
	// effect aux bus named "Hall" and its return channel "Hall Ret" both
	// contain "Hall", but only one of them should actually be hidden.
	//
	// A source not yet mentioned anywhere in the saved layout
	// (hub.buildConfig's natural-order fallback — same case LayoutItem's
	// doc comment describes) starts hidden instead of the usual
	// default-enabled if its name matches its type's pattern list — e.g.
	// "Master" in AutoHideGroupPatterns to keep a desk's Master/Main group
	// off the mixer page by default in every session, without re-hiding it
	// by hand each time. Applies only to that fallback default: a source
	// the admin has explicitly enabled stays enabled regardless of a later
	// pattern match, same as every other explicit-beats-default rule in
	// this admin UI. Global (applies across all sessions), not per-session
	// state, since the desk's channel-naming convention doesn't change
	// session to session.
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

// ExternalDevice is one relay target — TouchOSC, Reaper, StreamDeck, or
// similar — see the plan's external-device relay section. Broadcast gates
// whether it receives fan-out at all (it's effectively this device's
// enable/disable — see admin's per-row edit UI); Loopback controls whether
// it also receives messages that originated from itself (not yet
// meaningfully exercised — see hub's broadcastSource doc comment). ID is a
// stable admin-only identity (not used by hub, which only needs
// Name/Addr/Broadcast/Loopback for relay) so renaming a device in the admin
// UI doesn't invalidate its edit/delete URL.
type ExternalDevice struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IP        string `json:"ip"`
	Port      int    `json:"port"`
	Broadcast bool   `json:"broadcast"`
	Loopback  bool   `json:"loopback"`
}

// LayoutKindSource/LayoutKindSection are LayoutItem.Kind's two values.
const (
	LayoutKindSource  = "source"
	LayoutKindSection = "section"
)

// LayoutItem is one entry in State.MixerLayout — the single ordered
// sequence mixing sources and admin-created section markers, exactly as
// arranged in the admin UI's Sources tab. A source's section membership is
// derived from whichever marker precedes it in this same sequence, not
// stored as a field on the source — the old Title-on-a-source convention
// worked the same way (a header derived from position), just as a real
// named object now instead of a string glued onto whichever source
// happened to carry it.
//
// Array position IS display order: there is no separate Order field to
// keep in sync with it. That matters because two independently-numbered
// lists sharing an "order" axis by convention can silently drift apart
// (a bug, a hand-edit, two code paths assigning colliding values); a
// single ordered sequence structurally cannot, since there's only one
// thing recording order.
//
// Sparse, like the old Source overlay was: only a source the admin has
// actually customized (disabled, given an icon) or a section marker needs
// an entry here. Any channel/group the desk reports that isn't mentioned
// anywhere in MixerLayout is appended at the end, in natural desk order,
// by hub.buildConfig — same fallback this had before Sections existed.
type LayoutItem struct {
	Kind string `json:"kind"` // LayoutKindSource | LayoutKindSection

	// Set when Kind == LayoutKindSource.
	Type    string `json:"type"` // "channel" | "group"
	Number  int    `json:"number"`
	Enabled bool   `json:"enabled"`
	Icon    string `json:"icon"`

	// Set when Kind == LayoutKindSection.
	SectionID   string `json:"sectionId"`
	SectionName string `json:"sectionName"`
}

// State mirrors config/state.json — the admin-editable overlay merged onto
// live desk data by hub.buildConfig().
type State struct {
	UUID        string       `json:"uuid"`
	Name        string       `json:"name"`
	SessionName string       `json:"sessionName"`
	Auxes       []Aux        `json:"auxes"`
	MixerLayout []LayoutItem `json:"mixerLayout"`
}

// Aux is the admin-editable overlay for one aux bus, indexed 1:1 by
// position (aux N is Auxes[N-1]) since auxes stay the fixed destination
// concept — unlike sources/sections, which are reorderable/interleavable
// (see LayoutItem).
type Aux struct {
	Enabled bool   `json:"enabled"`
	Colour  string `json:"colour"`
	Icon    string `json:"icon"`
}

// Preset is a named, saved snapshot of State — mirrors JS's
// {...structuredClone(currentState), uuid, name, lastChanged} shape via
// embedding: State's own UUID/Name fields serve double duty as the
// preset's identity, matching the JS spread-and-override pattern exactly.
type Preset struct {
	State
	LastChanged string `json:"lastChanged"`
}
