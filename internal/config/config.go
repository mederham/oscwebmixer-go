package config

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	ErrPresetExists   = errors.New("a preset with that name already exists")
	ErrPresetNotFound = errors.New("preset not found")
)

type Manager struct {
	dir    string
	logger *log.Logger
}

func New(dir string, logger *log.Logger) *Manager {
	return &Manager{dir: dir, logger: logger}
}

func (m *Manager) globalPath() string  { return filepath.Join(m.dir, "global.json") }
func (m *Manager) statePath() string   { return filepath.Join(m.dir, "state.json") }
func (m *Manager) presetsPath() string { return filepath.Join(m.dir, "presets.json") }

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

func NewDefaultState() State {
	s := State{Name: "Default"}
	ensureDefaultLayout(&s)
	return s
}

func (m *Manager) EnsureDir() error {
	return os.MkdirAll(m.dir, 0o755)
}

func NewDefaultGlobal() Global {
	return Global{
		OSC:  OSCConfig{Port: 8000},
		Desk: DeskConfig{Port: 9000},
	}
}

func (m *Manager) LoadGlobalOrDefault() (Global, error) {
	data, err := os.ReadFile(m.globalPath())
	if errors.Is(err, os.ErrNotExist) {
		return NewDefaultGlobal(), nil
	}
	if err != nil {
		return Global{}, err
	}
	if len(data) == 0 {
		m.logger.Printf("config: %s is empty, using defaults", m.globalPath())
		return NewDefaultGlobal(), nil
	}
	var g Global
	if err := json.Unmarshal(data, &g); err != nil {
		return Global{}, fmt.Errorf("parse %s: %w", m.globalPath(), err)
	}
	return g, nil
}

func (m *Manager) SaveGlobal(g Global) error {
	return atomicWriteJSON(m.globalPath(), g)
}

func (m *Manager) LoadStateOrDefault() (State, error) {
	data, err := os.ReadFile(m.statePath())
	if errors.Is(err, os.ErrNotExist) {
		return NewDefaultState(), nil
	}
	if err != nil {
		return State{}, err
	}
	if len(data) == 0 {
		m.logger.Printf("config: %s is empty, using defaults", m.statePath())
		return NewDefaultState(), nil
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, fmt.Errorf("parse %s: %w", m.statePath(), err)
	}

	s.MixerLayouts = migrateLegacyLayout(data, s.MixerLayouts)
	ensureDefaultLayout(&s)
	return s, nil
}

// migrateLegacyLayout reads the old singular "mixerLayout" key some
// releases shipped, used when a state/preset file predates named layouts.
func migrateLegacyLayout(data []byte, layouts map[string]NamedLayout) map[string]NamedLayout {
	if len(layouts) > 0 {
		return layouts
	}
	var legacy struct {
		MixerLayout []LayoutItem `json:"mixerLayout"`
	}
	if json.Unmarshal(data, &legacy) == nil && len(legacy.MixerLayout) > 0 {
		return map[string]NamedLayout{DefaultLayoutID: {Name: "Default", Items: legacy.MixerLayout}}
	}
	return layouts
}

func (m *Manager) SaveState(s State) error {
	return atomicWriteJSON(m.statePath(), s)
}

func (m *Manager) LoadPresets() ([]Preset, error) {
	data, err := os.ReadFile(m.presetsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		m.logger.Printf("config: %s is empty, no presets loaded", m.presetsPath())
		return nil, nil
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

		presets[i].MixerLayouts = migrateLegacyLayout(r, presets[i].MixerLayouts)
		ensureDefaultLayout(&presets[i].State)
	}
	return presets, nil
}

func (m *Manager) savePresets(presets []Preset) error {
	return atomicWriteJSON(m.presetsPath(), presets)
}

func findPresetIndex(presets []Preset, uuid string) int {
	for i, p := range presets {
		if p.UUID == uuid {
			return i
		}
	}
	return -1
}

func (m *Manager) GetPresetByID(uuid string) (Preset, error) {
	presets, err := m.LoadPresets()
	if err != nil {
		return Preset{}, err
	}
	if idx := findPresetIndex(presets, uuid); idx != -1 {
		return presets[idx], nil
	}
	return Preset{}, ErrPresetNotFound
}

func presetNameTaken(presets []Preset, name string) bool {
	for _, p := range presets {
		if p.Name == name {
			return true
		}
	}
	return false
}

func uniqueName(presets []Preset, base string) string {
	if !presetNameTaken(presets, base) {
		return base
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s (%d)", base, i)
		if !presetNameTaken(presets, candidate) {
			return candidate
		}
	}
}

func (m *Manager) SaveCurrentStateAsPreset(state State, name string) (Preset, error) {
	presets, err := m.LoadPresets()
	if err != nil {
		return Preset{}, err
	}
	if presetNameTaken(presets, name) {
		return Preset{}, ErrPresetExists
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

func (m *Manager) ImportPresets(incoming []Preset) ([]Preset, error) {
	presets, err := m.LoadPresets()
	if err != nil {
		return nil, err
	}

	imported := make([]Preset, 0, len(incoming))
	for _, p := range incoming {
		id, err := NewID()
		if err != nil {
			return nil, err
		}
		name := strings.TrimSpace(p.Name)
		if name == "" {
			name = "Imported preset"
		}
		p.UUID = id
		p.Name = uniqueName(presets, name)
		p.LastChanged = time.Now().UTC().Format(time.RFC3339)
		ensureDefaultLayout(&p.State)

		presets = append(presets, p)
		imported = append(imported, p)
	}

	if err := m.savePresets(presets); err != nil {
		return nil, err
	}
	return imported, nil
}

func (m *Manager) UpdatePreset(uuid string, mixerLayouts map[string]NamedLayout, auxes []Aux) (Preset, error) {
	presets, err := m.LoadPresets()
	if err != nil {
		return Preset{}, err
	}
	idx := findPresetIndex(presets, uuid)
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

func (m *Manager) DeletePreset(uuid string) error {
	presets, err := m.LoadPresets()
	if err != nil {
		return err
	}
	idx := findPresetIndex(presets, uuid)
	if idx == -1 {
		return ErrPresetNotFound
	}
	presets = append(presets[:idx], presets[idx+1:]...)
	return m.savePresets(presets)
}

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
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}

	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
