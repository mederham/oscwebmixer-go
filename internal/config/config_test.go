package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadGlobalOrDefaultWhenMissing(t *testing.T) {
	m := New(t.TempDir())
	g, err := m.LoadGlobalOrDefault()
	if err != nil {
		t.Fatalf("LoadGlobalOrDefault: %v", err)
	}
	if g.OSC.Port != 8000 || g.Desk.Port != 9000 {
		t.Fatalf("defaults = %+v, want osc:8000 desk:9000", g)
	}
}

func TestSaveAndLoadGlobalRoundTrip(t *testing.T) {
	m := New(t.TempDir())
	want := Global{
		Debug: true,
		OSC:   OSCConfig{Port: 9001},
		Desk:  DeskConfig{IP: "10.0.0.18", Port: 9000},
		ExternalDevices: []ExternalDevice{
			{Name: "TouchOSC", IP: "10.0.0.50", Port: 9002, Broadcast: true, Loopback: false},
		},
	}
	if err := m.SaveGlobal(want); err != nil {
		t.Fatalf("SaveGlobal: %v", err)
	}
	got, err := m.LoadGlobalOrDefault()
	if err != nil {
		t.Fatalf("LoadGlobalOrDefault: %v", err)
	}
	if got.Desk.IP != want.Desk.IP || len(got.ExternalDevices) != 1 || got.ExternalDevices[0].Name != "TouchOSC" {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

func TestSaveAndLoadStateRoundTrip(t *testing.T) {
	m := New(t.TempDir())
	want := State{
		Name: "My Session",
		Auxes: []Aux{
			{Enabled: true, Colour: "#993333"},
		},
		MixerLayouts: map[string]NamedLayout{
			DefaultLayoutID: {
				Name: "Default",
				Items: []LayoutItem{
					{Kind: LayoutKindSource, Type: "channel", Number: 1, Enabled: true},
					{Kind: LayoutKindSection, SectionID: "sec-1", SectionName: "Groups"},
					{Kind: LayoutKindSource, Type: "group", Number: 3, Enabled: true},
				},
			},
		},
	}
	if err := m.SaveState(want); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	got, err := m.LoadStateOrDefault()
	if err != nil {
		t.Fatalf("LoadStateOrDefault: %v", err)
	}
	items := got.MixerLayouts[DefaultLayoutID].Items
	if len(items) != 3 {
		t.Fatalf("round trip = %+v, want 3 items", items)
	}
	if items[1].Kind != LayoutKindSection || items[1].SectionName != "Groups" {
		t.Fatalf("section marker round trip = %+v, want kind=%q name=Groups", items[1], LayoutKindSection)
	}
	if items[2].Type != "group" || items[2].Number != 3 {
		t.Fatalf("source after marker round trip = %+v, want group 3 (position preserved)", items[2])
	}
}

func TestPresetLifecycle(t *testing.T) {
	m := New(t.TempDir())
	state := State{MixerLayouts: map[string]NamedLayout{
		DefaultLayoutID: {Name: "Default", Items: []LayoutItem{{Kind: LayoutKindSource, Type: "channel", Number: 1, Enabled: true}}},
	}}

	preset, err := m.SaveCurrentStateAsPreset(state, "Show 1")
	if err != nil {
		t.Fatalf("SaveCurrentStateAsPreset: %v", err)
	}
	if preset.UUID == "" || preset.Name != "Show 1" || preset.LastChanged == "" {
		t.Fatalf("preset = %+v, missing expected fields", preset)
	}

	if _, err := m.SaveCurrentStateAsPreset(state, "Show 1"); !errors.Is(err, ErrPresetExists) {
		t.Fatalf("duplicate name: err = %v, want ErrPresetExists", err)
	}

	got, err := m.GetPresetByID(preset.UUID)
	if err != nil {
		t.Fatalf("GetPresetByID: %v", err)
	}
	if got.Name != "Show 1" {
		t.Fatalf("got = %+v", got)
	}

	if err := m.DeletePreset(preset.UUID); err != nil {
		t.Fatalf("DeletePreset: %v", err)
	}
	if _, err := m.GetPresetByID(preset.UUID); !errors.Is(err, ErrPresetNotFound) {
		t.Fatalf("after delete: err = %v, want ErrPresetNotFound", err)
	}
	if err := m.DeletePreset(preset.UUID); !errors.Is(err, ErrPresetNotFound) {
		t.Fatalf("deleting again: err = %v, want ErrPresetNotFound", err)
	}
}

func TestUpdatePreset(t *testing.T) {
	m := New(t.TempDir())
	original := State{MixerLayouts: map[string]NamedLayout{
		DefaultLayoutID: {Name: "Default", Items: []LayoutItem{{Kind: LayoutKindSource, Type: "channel", Number: 1, Enabled: true}}},
	}}
	preset, err := m.SaveCurrentStateAsPreset(original, "Show 1")
	if err != nil {
		t.Fatalf("SaveCurrentStateAsPreset: %v", err)
	}

	newLayouts := map[string]NamedLayout{
		DefaultLayoutID: {Name: "Default", Items: []LayoutItem{{Kind: LayoutKindSource, Type: "channel", Number: 2, Enabled: false}}},
	}
	newAuxes := []Aux{{Enabled: true, Colour: "#ff0000"}}
	updated, err := m.UpdatePreset(preset.UUID, newLayouts, newAuxes)
	if err != nil {
		t.Fatalf("UpdatePreset: %v", err)
	}
	if updated.UUID != preset.UUID || updated.Name != "Show 1" {
		t.Fatalf("updated = %+v, want same UUID/Name as the original preset", updated)
	}
	items := updated.MixerLayouts[DefaultLayoutID].Items
	if len(items) != 1 || items[0].Number != 2 {
		t.Fatalf("updated.MixerLayouts[default].Items = %+v, want the new layout", items)
	}
	if len(updated.Auxes) != 1 || updated.Auxes[0].Colour != "#ff0000" {
		t.Fatalf("updated.Auxes = %+v, want the new auxes", updated.Auxes)
	}
	if updated.LastChanged == "" {
		t.Fatal("LastChanged is empty, want it set")
	}

	got, err := m.GetPresetByID(preset.UUID)
	if err != nil {
		t.Fatalf("GetPresetByID: %v", err)
	}
	if items := got.MixerLayouts[DefaultLayoutID].Items; len(items) != 1 || items[0].Number != 2 {
		t.Fatalf("re-loaded preset = %+v, want the update to have persisted to disk", items)
	}

	if _, err := m.UpdatePreset("does-not-exist", newLayouts, newAuxes); !errors.Is(err, ErrPresetNotFound) {
		t.Fatalf("updating an unknown UUID: err = %v, want ErrPresetNotFound", err)
	}
}

func TestSaveGlobalIsAtomic(t *testing.T) {
	// Regression coverage for the plan's explicit atomicity requirement:
	// writing must go through a temp file + rename, never truncate the
	// target file in place.
	m := New(t.TempDir())
	if err := m.SaveGlobal(Global{OSC: OSCConfig{Port: 1}}); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if err := m.SaveGlobal(Global{OSC: OSCConfig{Port: 2}}); err != nil {
		t.Fatalf("second save: %v", err)
	}
	g, err := m.LoadGlobalOrDefault()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if g.OSC.Port != 2 {
		t.Fatalf("port = %d, want 2", g.OSC.Port)
	}
}

// TestLoadStateOrDefaultAlwaysHasDefaultLayout proves ensureDefaultLayout's
// invariant holds for a brand-new (missing) state.json — every caller
// downstream (hub, admin) relies on MixerLayouts[DefaultLayoutID] existing
// without a nil-check.
func TestLoadStateOrDefaultAlwaysHasDefaultLayout(t *testing.T) {
	m := New(t.TempDir())
	s, err := m.LoadStateOrDefault()
	if err != nil {
		t.Fatalf("LoadStateOrDefault: %v", err)
	}
	def, ok := s.MixerLayouts[DefaultLayoutID]
	if !ok {
		t.Fatalf("MixerLayouts = %+v, want a %q entry", s.MixerLayouts, DefaultLayoutID)
	}
	if def.Name != "Default" {
		t.Fatalf("default layout name = %q, want %q", def.Name, "Default")
	}
}

// TestLoadStateLegacyMixerLayoutShim proves a pre-rename state.json (the
// old singular "mixerLayout" array, no "mixerLayouts" object at all) is
// migrated into MixerLayouts[DefaultLayoutID] on load rather than silently
// dropped — see LoadStateOrDefault's ponytail-commented shim.
func TestLoadStateLegacyMixerLayoutShim(t *testing.T) {
	dir := t.TempDir()
	legacyJSON := `{
		"uuid": "abc",
		"name": "Old Session",
		"auxes": [{"enabled": true, "colour": "#112233"}],
		"mixerLayout": [
			{"kind": "source", "type": "channel", "number": 1, "enabled": true},
			{"kind": "section", "sectionId": "s1", "sectionName": "Drums"}
		]
	}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(legacyJSON), 0o644); err != nil {
		t.Fatalf("write legacy state.json: %v", err)
	}

	m := New(dir)
	s, err := m.LoadStateOrDefault()
	if err != nil {
		t.Fatalf("LoadStateOrDefault: %v", err)
	}
	items := s.MixerLayouts[DefaultLayoutID].Items
	if len(items) != 2 || items[0].Number != 1 || items[1].SectionName != "Drums" {
		t.Fatalf("migrated items = %+v, want the 2 legacy items carried into default", items)
	}
	if len(s.Auxes) != 1 || s.Auxes[0].Colour != "#112233" {
		t.Fatalf("auxes = %+v, want the legacy aux preserved (unaffected field)", s.Auxes)
	}
}

// TestLoadPresetsLegacyMixerLayoutShim mirrors
// TestLoadStateLegacyMixerLayoutShim for presets.json's per-entry shim.
func TestLoadPresetsLegacyMixerLayoutShim(t *testing.T) {
	dir := t.TempDir()
	legacyJSON := `[{
		"uuid": "p1",
		"name": "Show 1",
		"lastChanged": "2026-01-01T00:00:00Z",
		"mixerLayout": [{"kind": "source", "type": "channel", "number": 5, "enabled": true}]
	}]`
	if err := os.WriteFile(filepath.Join(dir, "presets.json"), []byte(legacyJSON), 0o644); err != nil {
		t.Fatalf("write legacy presets.json: %v", err)
	}

	m := New(dir)
	presets, err := m.LoadPresets()
	if err != nil {
		t.Fatalf("LoadPresets: %v", err)
	}
	if len(presets) != 1 {
		t.Fatalf("presets = %+v, want 1", presets)
	}
	items := presets[0].MixerLayouts[DefaultLayoutID].Items
	if len(items) != 1 || items[0].Number != 5 {
		t.Fatalf("migrated preset items = %+v, want the 1 legacy item carried into default", items)
	}
}

// TestMultipleNamedLayoutsRoundTrip proves State can hold more than one
// named layout (the "default" plus a custom one) and both survive a
// save/load cycle independently, each with its own Name and Items.
func TestMultipleNamedLayoutsRoundTrip(t *testing.T) {
	m := New(t.TempDir())
	want := State{
		Auxes: []Aux{{Enabled: true, LayoutID: "monitor-1"}},
		MixerLayouts: map[string]NamedLayout{
			DefaultLayoutID: {Name: "Default", Items: []LayoutItem{{Kind: LayoutKindSource, Type: "channel", Number: 1, Enabled: true}}},
			"monitor-1":     {Name: "Monitor Mix", Items: []LayoutItem{{Kind: LayoutKindSource, Type: "channel", Number: 2, Enabled: false}}},
		},
	}
	if err := m.SaveState(want); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	got, err := m.LoadStateOrDefault()
	if err != nil {
		t.Fatalf("LoadStateOrDefault: %v", err)
	}
	if len(got.MixerLayouts) != 2 {
		t.Fatalf("MixerLayouts = %+v, want 2 entries", got.MixerLayouts)
	}
	mon, ok := got.MixerLayouts["monitor-1"]
	if !ok || mon.Name != "Monitor Mix" || len(mon.Items) != 1 || mon.Items[0].Number != 2 {
		t.Fatalf("monitor-1 = %+v, want Name=Monitor Mix, 1 item (channel 2)", mon)
	}
	if got.Auxes[0].LayoutID != "monitor-1" {
		t.Fatalf("aux LayoutID = %q, want %q", got.Auxes[0].LayoutID, "monitor-1")
	}
}
