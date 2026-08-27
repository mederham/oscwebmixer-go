package admin

import (
	"fmt"

	"github.com/mederham/oscwebmixer-go/internal/config"
	"github.com/mederham/oscwebmixer-go/internal/hub"
	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
)

// ViewData is what the full index page renders.
type ViewData struct {
	Global      config.Global // external-devices-section reads this directly
	Network     NetworkSectionData
	SourcesData SourcesSectionData
	AuxesData   AuxesSectionData
	Presets     []PresetRow
	Message     string
}

// NetworkSectionData bundles the editable network settings with a live
// snapshot of desk connectivity (see hub.DeskStatusInfo) for the status
// dot — the two ride together since they render in the same section but
// come from different sources (config.Manager vs a hub query).
type NetworkSectionData struct {
	Global     config.Global
	DeskStatus hub.DeskStatusInfo
}

// SourcesSectionData carries an OOB flag so the same section template can
// render either as the primary htmx-targeted response or as an
// out-of-band fragment tacked onto another response (see
// handlePresetLoad: loading a preset changes both sections at once, but
// only one of them is the request's actual hx-target). Items is the flat,
// already-interleaved sequence — see oscmsg.LayoutItem's doc comment for
// why display order and section membership both come from list position,
// not a stored field, so there's nothing to group/sort here beyond a
// direct 1:1 conversion from the wire shape.
type SourcesSectionData struct {
	Items []SourceListItem
	OOB   bool
}

// SourceListItem is one row: exactly one of Section/Source is populated,
// selected by IsSection (an html/template range can't switch on a Go
// type, so this is the discriminator the template checks).
type SourceListItem struct {
	IsSection bool
	Section   SectionRow
	Source    SourceRow
}

type SectionRow struct {
	ID   string
	Name string
}

type AuxesSectionData struct {
	Auxes []AuxRow
	Icons []string // available aux icon picker options — see Handler.icons
	OOB   bool
}

// SourceRow is one card.
type SourceRow struct {
	Type    string
	Number  int
	Badge   string // "Ch 01" / "Grp 01"
	Label   string
	Enabled bool
	Icon    string
}

// Key identifies this card in admin.js's collectLayout() — data-type/
// data-number attributes, not a form field name (there's no per-card
// order/section field to name anymore; the whole list's order and each
// item's current field values are read fresh from the DOM on submit).
func (r SourceRow) Key() string { return fmt.Sprintf("%s:%d", r.Type, r.Number) }

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

func badge(sourceType string, number int) string {
	prefix := "Ch"
	if sourceType == "group" {
		prefix = "Grp"
	}
	return fmt.Sprintf("%s %02d", prefix, number)
}

// toSourcesSectionData is a direct, order-preserving conversion from the
// wire shape to what the template renders — buildConfig already produced
// MixerLayout in final display order, so there's no merging/sorting to do
// here, just mapping each item to its row shape.
func toSourcesSectionData(layout []oscmsg.LayoutItem, oob bool) SourcesSectionData {
	items := make([]SourceListItem, len(layout))
	for i, it := range layout {
		if it.Kind == oscmsg.LayoutKindSection {
			items[i] = SourceListItem{IsSection: true, Section: SectionRow{ID: it.SectionID, Name: it.SectionName}}
			continue
		}
		items[i] = SourceListItem{Source: SourceRow{
			Type: it.Type, Number: it.Number, Badge: badge(it.Type, it.Number),
			Label: it.Label, Enabled: it.Enabled, Icon: it.Icon,
		}}
	}
	return SourcesSectionData{Items: items, OOB: oob}
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

// layoutFromPayload converts the wire LayoutItem shape into what's
// persisted to config/state.json, stripping live/desk-derived fields
// (Label, Stereo) that don't belong on disk.
func layoutFromPayload(layout []oscmsg.LayoutItem) []config.LayoutItem {
	out := make([]config.LayoutItem, len(layout))
	for i, it := range layout {
		if it.Kind == oscmsg.LayoutKindSection {
			out[i] = config.LayoutItem{Kind: config.LayoutKindSection, SectionID: it.SectionID, SectionName: it.SectionName}
			continue
		}
		out[i] = config.LayoutItem{
			Kind: config.LayoutKindSource, Type: it.Type, Number: it.Number,
			Enabled: it.Enabled, Icon: it.Icon,
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
