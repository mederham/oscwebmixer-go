package admin

import (
	"github.com/mederham/oscwebmixer-go/internal/config"
	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
)

// ViewData is what the full index page renders.
type ViewData struct {
	Global      config.Global
	SourcesData SourcesSectionData
	AuxesData   AuxesSectionData
	Presets     []PresetRow
	Message     string
}

// SourcesSectionData/AuxesSectionData carry an OOB flag so the same
// section template can render either as the primary htmx-targeted
// response or as an out-of-band fragment tacked onto another response
// (see handlePresetLoad: loading a preset changes both sections at once,
// but only one of them is the request's actual hx-target).
type SourcesSectionData struct {
	Sources []SourceRow
	OOB     bool
}

type AuxesSectionData struct {
	Auxes []AuxRow
	OOB   bool
}

type SourceRow struct {
	Type    string
	Number  int
	Label   string
	Enabled bool
	Order   int
	Title   string
	Icon    string
}

type AuxRow struct {
	Channel int
	Label   string
	Enabled bool
	Stereo  bool
	Colour  string
	Icon    string
}

type PresetRow struct {
	UUID        string
	Name        string
	LastChanged string
}

func toSourceRows(sources []oscmsg.SourceConfig) []SourceRow {
	rows := make([]SourceRow, len(sources))
	for i, s := range sources {
		rows[i] = SourceRow{
			Type: s.Type, Number: s.Number, Label: s.Label,
			Enabled: s.Enabled, Order: s.Order, Title: s.Title, Icon: s.Icon,
		}
	}
	return rows
}

func toAuxRows(auxes []oscmsg.AuxConfig) []AuxRow {
	rows := make([]AuxRow, len(auxes))
	for i, a := range auxes {
		rows[i] = AuxRow{
			Channel: a.Channel, Label: a.Label, Enabled: a.Enabled,
			Stereo: a.Stereo, Colour: a.Colour, Icon: a.Icon,
		}
	}
	return rows
}

func toPresetRows(presets []config.Preset) []PresetRow {
	rows := make([]PresetRow, len(presets))
	for i, p := range presets {
		rows[i] = PresetRow{UUID: p.UUID, Name: p.Name, LastChanged: p.LastChanged}
	}
	return rows
}

// configSourcesFromPayload strips the live/desk-derived Label field,
// keeping only what's actually persisted to config/state.json.
func configSourcesFromPayload(sources []oscmsg.SourceConfig) []config.Source {
	out := make([]config.Source, len(sources))
	for i, s := range sources {
		out[i] = config.Source{
			Type: s.Type, Number: s.Number, Enabled: s.Enabled,
			DisplayOrder: s.Order, Title: s.Title, Icon: s.Icon,
		}
	}
	return out
}

// configAuxesFromPayload relies on cfg.Auxes always being in channel order
// (buildConfig constructs it that way — Auxes[i].Channel == i+1), matching
// config.Aux's own indexed-by-position convention.
func configAuxesFromPayload(auxes []oscmsg.AuxConfig) []config.Aux {
	out := make([]config.Aux, len(auxes))
	for i, a := range auxes {
		out[i] = config.Aux{Enabled: a.Enabled, Colour: a.Colour, Icon: a.Icon}
	}
	return out
}
