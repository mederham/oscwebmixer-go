package admin

import (
	"fmt"
	"sort"

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
	Version     string
}

// NetworkSectionData bundles the editable network settings with a live
// snapshot of desk connectivity (see hub.DeskStatusInfo) for the status
// dot — the two ride together since they render in the same section but
// come from different sources (config.Manager vs a hub query).
type NetworkSectionData struct {
	Global     config.Global
	DeskStatus hub.DeskStatusInfo
}

// LayoutOption is one entry in a layout picker — the Sources tab's
// layout `<select>` and the Auxes tab's per-aux layout `<select>` both
// render from the same list (see layoutOptions).
type LayoutOption struct {
	ID   string
	Name string
}

// SourcesSectionData carries an OOB flag so the same section template can
// render either as the primary htmx-targeted response or as an
// out-of-band fragment tacked onto another response (see
// handlePresetLoad: loading a preset changes both sections at once, but
// only one of them is the request's actual hx-target). Items is the flat,
// already-interleaved sequence for whichever layout LayoutID names — see
// oscmsg.LayoutItem's doc comment for why display order and section
// membership both come from list position, not a stored field, so
// there's nothing to group/sort here beyond a direct 1:1 conversion from
// the wire shape. Layouts is every layout's id+name, for the picker.
type SourcesSectionData struct {
	Items      []SourceListItem
	LayoutID   string
	LayoutName string
	IsDefault  bool // layoutID == oscmsg.DefaultLayoutID — the rename/delete controls hide for it
	Layouts    []LayoutOption
	OOB        bool
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
	Auxes   []AuxRow
	Icons   []string // available aux icon picker options — see Handler.icons
	Layouts []LayoutOption
	OOB     bool
}

// SourceRow is one card. No Badge field — "Ch 01"/"Grp 01" is derived from
// Type+Number by the badge template func at render time (see admin.go),
// not stored here where it could drift out of sync with them. Stereo
// drives the card's double-line border (see admin.css) — the same visual
// system as the type-color border, not a separate text indicator.
type SourceRow struct {
	Type    string
	Number  int
	Label   string
	Enabled bool
	Stereo  bool
	Icon    string
}

// Key identifies this card in admin.js's collectLayout() — data-type/
// data-number attributes, not a form field name (there's no per-card
// order/section field to name anymore; the whole list's order and each
// item's current field values are read fresh from the DOM on submit).
func (r SourceRow) Key() string { return fmt.Sprintf("%s:%d", r.Type, r.Number) }

// AuxRow is one card. No Badge field — "Aux N" is just Channel, rendered
// directly in the template (auxes.html.tmpl).
type AuxRow struct {
	Channel  int
	Label    string
	Enabled  bool
	Stereo   bool
	Colour   string
	Icon     string
	LayoutID string
}

// Active is derived, not stored: loading a preset copies its UUID into
// state.json wholesale (see handlePresetLoad), and ordinary edits
// (saveState) never touch that field — so "does this preset's UUID match
// the live state's UUID" is already a reliable "is this the currently
// loaded preset" signal with no new schema needed. It stops matching the
// moment a different preset is loaded, or state is reset (both assign a
// different/empty UUID).
type PresetRow struct {
	UUID        string
	Name        string
	LastChanged string
	Active      bool
}

func badge(sourceType string, number int) string {
	prefix := "Ch"
	if sourceType == "group" {
		prefix = "Grp"
	}
	return fmt.Sprintf("%s %02d", prefix, number)
}

// toSourcesSectionData is a direct, order-preserving conversion from the
// wire shape to what the template renders for one layout's items —
// buildConfig already produced them in final display order, so there's
// no merging/sorting to do here, just mapping each item to its row shape.
func toSourcesSectionData(items []oscmsg.LayoutItem, layoutID, layoutName string, layouts []LayoutOption, oob bool) SourcesSectionData {
	rows := make([]SourceListItem, len(items))
	for i, it := range items {
		if it.Kind == oscmsg.LayoutKindSection {
			rows[i] = SourceListItem{IsSection: true, Section: SectionRow{ID: it.SectionID, Name: it.SectionName}}
			continue
		}
		rows[i] = SourceListItem{Source: SourceRow{
			Type: it.Type, Number: it.Number,
			Label: it.Label, Enabled: it.Enabled, Stereo: it.Stereo, Icon: it.Icon,
		}}
	}
	return SourcesSectionData{
		Items: rows, LayoutID: layoutID, LayoutName: layoutName,
		IsDefault: layoutID == oscmsg.DefaultLayoutID,
		Layouts:   layouts, OOB: oob,
	}
}

// layoutOptions returns every layout's id+name from a queried config,
// "default" first (however it's currently named) then the rest sorted by
// name — feeds the Sources tab picker and the Auxes tab's per-aux select.
func layoutOptions(layouts map[string]oscmsg.NamedLayout) []LayoutOption {
	opts := make([]LayoutOption, 0, len(layouts))
	for id, nl := range layouts {
		if id == oscmsg.DefaultLayoutID {
			continue
		}
		opts = append(opts, LayoutOption{ID: id, Name: nl.Name})
	}
	sort.Slice(opts, func(i, j int) bool {
		if opts[i].Name != opts[j].Name {
			return opts[i].Name < opts[j].Name
		}
		return opts[i].ID < opts[j].ID
	})
	name := "Default"
	if nl, ok := layouts[oscmsg.DefaultLayoutID]; ok && nl.Name != "" {
		name = nl.Name
	}
	return append([]LayoutOption{{ID: oscmsg.DefaultLayoutID, Name: name}}, opts...)
}

// sourcesSectionData/auxesSectionData bundle a queried ConfigPayload into
// what each section template renders — shared by handleIndex (the full
// page) and render.go's renderSourcesSection/renderAuxesSection (htmx
// partial swaps), so the two never drift on which fields get populated.
func sourcesSectionData(cfg oscmsg.ConfigPayload, layoutID string, oob bool) SourcesSectionData {
	nl := cfg.MixerLayouts[layoutID]
	return toSourcesSectionData(nl.Items, layoutID, nl.Name, layoutOptions(cfg.MixerLayouts), oob)
}

func auxesSectionData(cfg oscmsg.ConfigPayload, icons []string, oob bool) AuxesSectionData {
	return AuxesSectionData{Auxes: toAuxRows(cfg.Auxes), Icons: icons, Layouts: layoutOptions(cfg.MixerLayouts), OOB: oob}
}

func toAuxRows(auxes []oscmsg.AuxConfig) []AuxRow {
	rows := make([]AuxRow, len(auxes))
	for i, a := range auxes {
		rows[i] = AuxRow{
			Channel: a.Channel, Label: a.Label, Enabled: a.Enabled,
			Stereo: a.Stereo, Colour: a.Colour, Icon: a.Icon, LayoutID: a.LayoutID,
		}
	}
	return rows
}

func toPresetRows(presets []config.Preset, activeUUID string) []PresetRow {
	rows := make([]PresetRow, len(presets))
	for i, p := range presets {
		rows[i] = PresetRow{
			UUID: p.UUID, Name: p.Name, LastChanged: p.LastChanged,
			Active: activeUUID != "" && p.UUID == activeUUID,
		}
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
		out[i] = config.Aux{Enabled: a.Enabled, Colour: a.Colour, Icon: a.Icon, LayoutID: a.LayoutID}
	}
	return out
}

// configLayoutsFromPayload converts every named layout in a queried
// ConfigPayload into what's persisted to config/state.json — the preset
// handlers' (Save/Update/Load/Reset) whole-map counterpart to
// layoutFromPayload's single-layout conversion, since a preset snapshots
// every layout at once, not just whichever one the Sources tab currently
// has open.
func configLayoutsFromPayload(layouts map[string]oscmsg.NamedLayout) map[string]config.NamedLayout {
	out := make(map[string]config.NamedLayout, len(layouts))
	for id, nl := range layouts {
		out[id] = config.NamedLayout{Name: nl.Name, Items: layoutFromPayload(nl.Items)}
	}
	return out
}
