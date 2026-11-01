package admin

import (
	"fmt"
	"sort"

	"github.com/mederham/oscwebmixer-go/internal/config"
	"github.com/mederham/oscwebmixer-go/internal/hub"
	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
)

type ViewData struct {
	Global      config.Global
	Network     NetworkSectionData
	SourcesData SourcesSectionData
	AuxesData   AuxesSectionData
	Presets     []PresetRow
	Message     string
	Version     string
}

type NetworkSectionData struct {
	Global     config.Global
	DeskStatus hub.DeskStatusInfo
}

type LayoutOption struct {
	ID   string
	Name string
}

type SourcesSectionData struct {
	Items      []SourceListItem
	LayoutID   string
	LayoutName string
	IsDefault  bool
	Layouts    []LayoutOption
	OOB        bool
}

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
	Icons   []string
	Layouts []LayoutOption
	OOB     bool
}

type SourceRow struct {
	Type    string
	Number  int
	Label   string
	Enabled bool
	Stereo  bool
}

type AuxRow struct {
	Channel  int
	Label    string
	Enabled  bool
	Stereo   bool
	Colour   string
	Icon     string
	LayoutID string
}

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

func toSourcesSectionData(items []oscmsg.LayoutItem, layoutID, layoutName string, layouts []LayoutOption, oob bool) SourcesSectionData {
	rows := make([]SourceListItem, len(items))
	for i, it := range items {
		if it.Kind == oscmsg.LayoutKindSection {
			rows[i] = SourceListItem{IsSection: true, Section: SectionRow{ID: it.SectionID, Name: it.SectionName}}
			continue
		}
		rows[i] = SourceListItem{Source: SourceRow{
			Type: it.Type, Number: it.Number,
			Label: it.Label, Enabled: it.Enabled, Stereo: it.Stereo,
		}}
	}
	return SourcesSectionData{
		Items: rows, LayoutID: layoutID, LayoutName: layoutName,
		IsDefault: layoutID == oscmsg.DefaultLayoutID,
		Layouts:   layouts, OOB: oob,
	}
}

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

func layoutFromPayload(layout []oscmsg.LayoutItem) []config.LayoutItem {
	out := make([]config.LayoutItem, len(layout))
	for i, it := range layout {
		if it.Kind == oscmsg.LayoutKindSection {
			out[i] = config.LayoutItem{Kind: config.LayoutKindSection, SectionID: it.SectionID, SectionName: it.SectionName}
			continue
		}
		out[i] = config.LayoutItem{
			Kind: config.LayoutKindSource, Type: it.Type, Number: it.Number,
			Enabled: it.Enabled,
		}
	}
	return out
}

func configAuxesFromPayload(auxes []oscmsg.AuxConfig) []config.Aux {
	out := make([]config.Aux, len(auxes))
	for i, a := range auxes {
		out[i] = config.Aux{Enabled: a.Enabled, Colour: a.Colour, Icon: a.Icon, LayoutID: a.LayoutID}
	}
	return out
}

func configLayoutsFromPayload(layouts map[string]oscmsg.NamedLayout) map[string]config.NamedLayout {
	out := make(map[string]config.NamedLayout, len(layouts))
	for id, nl := range layouts {
		out[id] = config.NamedLayout{Name: nl.Name, Items: layoutFromPayload(nl.Items)}
	}
	return out
}
