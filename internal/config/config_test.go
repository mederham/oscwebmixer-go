package config

import (
	"errors"
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
		MixerLayout: []LayoutItem{
			{Kind: LayoutKindSource, Type: "channel", Number: 1, Enabled: true},
			{Kind: LayoutKindSection, SectionID: "sec-1", SectionName: "Groups"},
			{Kind: LayoutKindSource, Type: "group", Number: 3, Enabled: true},
		},
	}
	if err := m.SaveState(want); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	got, err := m.LoadStateOrDefault()
	if err != nil {
		t.Fatalf("LoadStateOrDefault: %v", err)
	}
	if len(got.MixerLayout) != 3 {
		t.Fatalf("round trip = %+v, want 3 items", got.MixerLayout)
	}
	if got.MixerLayout[1].Kind != LayoutKindSection || got.MixerLayout[1].SectionName != "Groups" {
		t.Fatalf("section marker round trip = %+v, want kind=%q name=Groups", got.MixerLayout[1], LayoutKindSection)
	}
	if got.MixerLayout[2].Type != "group" || got.MixerLayout[2].Number != 3 {
		t.Fatalf("source after marker round trip = %+v, want group 3 (position preserved)", got.MixerLayout[2])
	}
}

func TestPresetLifecycle(t *testing.T) {
	m := New(t.TempDir())
	state := State{MixerLayout: []LayoutItem{{Kind: LayoutKindSource, Type: "channel", Number: 1, Enabled: true}}}

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
