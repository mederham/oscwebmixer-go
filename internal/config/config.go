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
			Server: ServerConfig{Port: 80},
			OSC:    OSCConfig{Port: 8000},
			Desk:   DeskConfig{Port: 9000},
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
		return State{Name: "Default"}, nil
	}
	if err != nil {
		return State{}, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, fmt.Errorf("parse %s: %w", m.statePath(), err)
	}
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
	var presets []Preset
	if err := json.Unmarshal(data, &presets); err != nil {
		return nil, fmt.Errorf("parse %s: %w", m.presetsPath(), err)
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
