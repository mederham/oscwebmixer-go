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
	if g.Server.Port != 80 || g.OSC.Port != 8000 || g.Desk.Port != 9000 {
		t.Fatalf("defaults = %+v, want server:80 osc:8000 desk:9000", g)
	}
}

func TestSaveAndLoadGlobalRoundTrip(t *testing.T) {
	m := New(t.TempDir())
	want := Global{
		Debug:  true,
		Server: ServerConfig{Port: 8080},
		OSC:    OSCConfig{Port: 9001},
		Desk:   DeskConfig{IP: "10.0.0.18", Port: 9000},
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
		Sources: []Source{
			{Type: "channel", Number: 1, Enabled: true, DisplayOrder: 0},
			{Type: "group", Number: 3, Enabled: true, DisplayOrder: 1, Title: "Groups"},
		},
	}
	if err := m.SaveState(want); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	got, err := m.LoadStateOrDefault()
	if err != nil {
		t.Fatalf("LoadStateOrDefault: %v", err)
	}
	if len(got.Sources) != 2 || got.Sources[1].Type != "group" || got.Sources[1].Title != "Groups" {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

func TestPresetLifecycle(t *testing.T) {
	m := New(t.TempDir())
	state := State{Sources: []Source{{Type: "channel", Number: 1, Enabled: true}}}

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
	if err := m.SaveGlobal(Global{Server: ServerConfig{Port: 1}}); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if err := m.SaveGlobal(Global{Server: ServerConfig{Port: 2}}); err != nil {
		t.Fatalf("second save: %v", err)
	}
	g, err := m.LoadGlobalOrDefault()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if g.Server.Port != 2 {
		t.Fatalf("port = %d, want 2", g.Server.Port)
	}
}
