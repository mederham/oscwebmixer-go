package config

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

var (
	// ErrPresetExists is returned by SaveCurrentStateAsPreset when a
	// preset with that name already exists.
	ErrPresetExists = errors.New("a preset with that name already exists")
	// ErrPresetNotFound is returned by GetPresetByID/DeletePreset.
	ErrPresetNotFound = errors.New("preset not found")
)

// Manager reads/writes the JSON config files under a directory (normally
// "./config", but injectable for tests).
type Manager struct {
	dir string
}

// New builds a Manager rooted at dir (e.g. "./config").
func New(dir string) *Manager {
	return &Manager{dir: dir}
}

func (m *Manager) globalPath() string  { return filepath.Join(m.dir, "global.json") }
func (m *Manager) statePath() string   { return filepath.Join(m.dir, "state.json") }
func (m *Manager) presetsPath() string { return filepath.Join(m.dir, "presets.json") }

// ensureDefaultLayout guarantees s.MixerLayouts has a DefaultLayoutID
// entry named "Default" — called after every load (fresh, legacy-shimmed,
// or already-current) so every other function in this package and its
// callers can rely on that entry existing without a nil-check.
func ensureDefaultLayout(s *State) {
	if s.MixerLayouts == nil {
		s.MixerLayouts = map[string]NamedLayout{}
	}
	def, ok := s.MixerLayouts[DefaultLayoutID]
	if !ok || def.Name == "" {
		def.Name = "Default"
		s.MixerLayouts[DefaultLayoutID] = def
	}
}

// NewDefaultState returns a blank State with the "default" layout already
// present — the zero-value starting point both a missing state.json
// (LoadStateOrDefault) and an explicit "reset everything" admin action
// (handlePresetsReset) share, so the invariant lives in exactly one place.
func NewDefaultState() State {
	s := State{Name: "Default"}
	ensureDefaultLayout(&s)
	return s
}

// EnsureDir creates the config directory if it doesn't exist yet.
func (m *Manager) EnsureDir() error {
	return os.MkdirAll(m.dir, 0o755)
}

// LoadGlobalOrDefault loads global.json, or returns sensible defaults if
// it doesn't exist yet (mirrors configManager.js's getGlobalConfigOrDefault).
func (m *Manager) LoadGlobalOrDefault() (Global, error) {
	data, err := os.ReadFile(m.globalPath())
	if errors.Is(err, os.ErrNotExist) {
		return Global{
			OSC:  OSCConfig{Port: 8000},
			Desk: DeskConfig{Port: 9000},
		}, nil
	}
	if err != nil {
		return Global{}, err
	}
	var g Global
	if err := json.Unmarshal(data, &g); err != nil {
		return Global{}, fmt.Errorf("parse %s: %w", m.globalPath(), err)
	}
	return g, nil
}

// SaveGlobal atomically writes global.json.
func (m *Manager) SaveGlobal(g Global) error {
	return atomicWriteJSON(m.globalPath(), g)
}

// LoadStateOrDefault loads state.json, or an empty default if it doesn't
// exist yet (mirrors configManager.js's getCurrentStateOrDefault).
func (m *Manager) LoadStateOrDefault() (State, error) {
	data, err := os.ReadFile(m.statePath())
	if errors.Is(err, os.ErrNotExist) {
		return NewDefaultState(), nil
	}
	if err != nil {
		return State{}, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, fmt.Errorf("parse %s: %w", m.statePath(), err)
	}
	// ponytail: one-time compat shim for the mixerLayout -> mixerLayouts
	// rename (2026-09). Safe to delete once you're confident every
	// state.json on disk has been resaved in the new shape (happens
	// automatically on the first admin edit after upgrading) — see the
	// design doc's "legacy compat shim" section for why this isn't a
	// general migration framework.
	if len(s.MixerLayouts) == 0 {
		var legacy struct {
			MixerLayout []LayoutItem `json:"mixerLayout"`
		}
		if json.Unmarshal(data, &legacy) == nil && len(legacy.MixerLayout) > 0 {
			s.MixerLayouts = map[string]NamedLayout{DefaultLayoutID: {Name: "Default", Items: legacy.MixerLayout}}
		}
	}
	ensureDefaultLayout(&s)
	return s, nil
}

// SaveState atomically writes state.json.
func (m *Manager) SaveState(s State) error {
	return atomicWriteJSON(m.statePath(), s)
}

// LoadPresets loads presets.json, or an empty list if it doesn't exist yet.
func (m *Manager) LoadPresets() ([]Preset, error) {
	data, err := os.ReadFile(m.presetsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", m.presetsPath(), err)
	}
	presets := make([]Preset, len(raw))
	for i, r := range raw {
		if err := json.Unmarshal(r, &presets[i]); err != nil {
			return nil, fmt.Errorf("parse %s: %w", m.presetsPath(), err)
		}
		// ponytail: same one-time compat shim as LoadStateOrDefault,
		// applied per preset entry — see that function's doc comment.
		if len(presets[i].MixerLayouts) == 0 {
			var legacy struct {
				MixerLayout []LayoutItem `json:"mixerLayout"`
			}
			if json.Unmarshal(r, &legacy) == nil && len(legacy.MixerLayout) > 0 {
				presets[i].MixerLayouts = map[string]NamedLayout{DefaultLayoutID: {Name: "Default", Items: legacy.MixerLayout}}
			}
		}
		ensureDefaultLayout(&presets[i].State)
	}
	return presets, nil
}

func (m *Manager) savePresets(presets []Preset) error {
	return atomicWriteJSON(m.presetsPath(), presets)
}

// GetPresetByID returns the preset with the given uuid, or
// ErrPresetNotFound.
func (m *Manager) GetPresetByID(uuid string) (Preset, error) {
	presets, err := m.LoadPresets()
	if err != nil {
		return Preset{}, err
	}
	for _, p := range presets {
		if p.UUID == uuid {
			return p, nil
		}
	}
	return Preset{}, ErrPresetNotFound
}

// SaveCurrentStateAsPreset snapshots state under a new name. Returns
// ErrPresetExists if the name is already taken (mirrors
// configManager.js's uniqueness check).
func (m *Manager) SaveCurrentStateAsPreset(state State, name string) (Preset, error) {
	presets, err := m.LoadPresets()
	if err != nil {
		return Preset{}, err
	}
	for _, p := range presets {
		if p.Name == name {
			return Preset{}, ErrPresetExists
		}
	}

	id, err := NewID()
	if err != nil {
		return Preset{}, err
	}
	preset := Preset{State: state, LastChanged: time.Now().UTC().Format(time.RFC3339)}
	preset.UUID = id
	preset.Name = name

	presets = append(presets, preset)
	if err := m.savePresets(presets); err != nil {
		return Preset{}, err
	}
	return preset, nil
}

// UpdatePreset overwrites an existing preset's saved layouts/auxes in
// place — same identity (UUID, Name), fresh content and LastChanged. The
// admin panel's "Update" action on the currently active preset — the
// counterpart to Load, which goes the other direction (discard live
// edits, revert to what's saved) — distinct from
// SaveCurrentStateAsPreset, which always creates a new preset under a new
// name. Returns ErrPresetNotFound if uuid doesn't match any saved preset.
func (m *Manager) UpdatePreset(uuid string, mixerLayouts map[string]NamedLayout, auxes []Aux) (Preset, error) {
	presets, err := m.LoadPresets()
	if err != nil {
		return Preset{}, err
	}
	idx := -1
	for i, p := range presets {
		if p.UUID == uuid {
			idx = i
			break
		}
	}
	if idx == -1 {
		return Preset{}, ErrPresetNotFound
	}
	presets[idx].MixerLayouts = mixerLayouts
	presets[idx].Auxes = auxes
	presets[idx].LastChanged = time.Now().UTC().Format(time.RFC3339)
	if err := m.savePresets(presets); err != nil {
		return Preset{}, err
	}
	return presets[idx], nil
}

// DeletePreset removes a preset by uuid, or returns ErrPresetNotFound.
func (m *Manager) DeletePreset(uuid string) error {
	presets, err := m.LoadPresets()
	if err != nil {
		return err
	}
	idx := -1
	for i, p := range presets {
		if p.UUID == uuid {
			idx = i
			break
		}
	}
	if idx == -1 {
		return ErrPresetNotFound
	}
	presets = append(presets[:idx], presets[idx+1:]...)
	return m.savePresets(presets)
}

// atomicWriteJSON writes to a temp file in the same directory then renames
// into place, so a crash mid-write can never corrupt the target file (the
// JS version's fs.writeFileSync had no such atomicity — see plan).
func atomicWriteJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// NewID generates a random UUIDv4 without pulling in an external
// dependency for something this small — used for preset, section, and
// external-device identity alike.
func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
