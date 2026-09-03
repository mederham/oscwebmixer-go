# Aux-Specific Mixer Layouts Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the admin define multiple named mixer layouts, assign one to each aux (default: the "Default" layout), and have the mixer client render the right layout when a user switches aux — persisted in both `state.json` and `presets.json`.

**Architecture:** `mixerLayout []LayoutItem` becomes `mixerLayouts map[string]NamedLayout` end to end (config → hub → wire → admin UI → mixer client). The server pushes every named layout to every client in one payload (unchanged broadcast model); the mixer client picks which one to render locally based on the selected aux's `layoutId`, avoiding any new per-client server logic.

**Tech Stack:** Go 1.22+ stdlib `net/http`/`encoding/json`, htmx (declarative, server-rendered admin UI), vanilla JS (`web/mixer/mixer.js`, `web/admin/admin.js`), Go's `html/template`.

**Spec:** `docs/superpowers/specs/2026-09-03-aux-specific-layouts-design.md`

## Global Constraints

- Layout identity is a stable id (`config.NewID()`, same convention as sections/presets/devices) + a separate `Name` — never rename-by-changing-the-key.
- The `"default"` layout id is always present, never renamed, never deleted.
- A new layout clones the layout currently being viewed when created (not empty).
- Deleting a layout in use by an aux does **not** block or warn — that aux silently falls back to `"default"` (a missing map key), same as an aux that never had a custom layout.
- Include the legacy `state.json`/`presets.json` compat shim (old `"mixerLayout": [...]` key), marked with a `ponytail:` comment as a one-time, non-self-cleaning compat path.
- No general config-version migration framework — out of scope, discussed and explicitly deferred in the spec.
- Bump `VERSION` (semver) at the end, per standing project convention (memory: auto-bump-version).

---

## Task 1: Config data model — `NamedLayout`, `MixerLayouts`, `Aux.LayoutID`, legacy shim

**Files:**
- Modify: `internal/config/types.go`
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.NamedLayout{Name string, Items []LayoutItem}`, `config.State.MixerLayouts map[string]NamedLayout`, `config.Aux.LayoutID string`, `config.DefaultLayoutID = "default"`, `config.NewDefaultState() State`, `(*Manager).UpdatePreset(uuid string, mixerLayouts map[string]NamedLayout, auxes []Aux) (Preset, error)`.
- Consumes: nothing new (only stdlib `encoding/json`, already imported).

- [ ] **Step 1: Update `types.go` — add `NamedLayout`, rename `State.MixerLayout`, add `Aux.LayoutID`**

In `internal/config/types.go`, add the `DefaultLayoutID` const near `LayoutKindSource`/`LayoutKindSection`, add `NamedLayout`, and change `State`/`Aux`:

```go
// LayoutKindSource/LayoutKindSection are LayoutItem.Kind's two values.
const (
	LayoutKindSource  = "source"
	LayoutKindSection = "section"
)

// DefaultLayoutID is the one map key in State.MixerLayouts guaranteed to
// always exist — every Manager load enforces this (see ensureDefaultLayout)
// so callers elsewhere never need to nil-check or fall back by hand. It's
// a fixed literal, not a generated id like every other layout gets,
// specifically so it's recognizable in code and never renamed/deleted (see
// admin's handleLayoutRename/handleLayoutDelete guards).
const DefaultLayoutID = "default"
```

Replace `LayoutItem`'s doc comment reference and add `NamedLayout` right after it (leave `LayoutItem` itself unchanged — only its doc comment's `State.MixerLayout` mention needs updating to `State.MixerLayouts`):

```go
// LayoutItem is one entry in a NamedLayout's Items — the single ordered
// sequence mixing sources and admin-created section markers, exactly as
// arranged in the admin UI's Sources tab for that named layout. A source's
// section membership is derived from whichever marker precedes it in this
// same sequence, not stored as a field on the source — the old Title-on-a-
// source convention worked the same way (a header derived from position),
// just as a real named object now instead of a string glued onto whichever
// source happened to carry it.
//
// Array position IS display order: there is no separate Order field to
// keep in sync with it. That matters because two independently-numbered
// lists sharing an "order" axis by convention can silently drift apart
// (a bug, a hand-edit, two code paths assigning colliding values); a
// single ordered sequence structurally cannot, since there's only one
// thing recording order.
//
// Sparse, like the old Source overlay was: only a source the admin has
// actually customized (disabled, given an icon) or a section marker needs
// an entry here. Any channel/group the desk reports that isn't mentioned
// anywhere in a given layout's Items is appended at the end, in natural
// desk order, by hub.buildConfig — same fallback this had before Sections
// existed, now applied independently per named layout.
type LayoutItem struct {
	Kind string `json:"kind"` // LayoutKindSource | LayoutKindSection

	// Set when Kind == LayoutKindSource.
	Type    string `json:"type"` // "channel" | "group"
	Number  int    `json:"number"`
	Enabled bool   `json:"enabled"`
	Icon    string `json:"icon"`

	// Set when Kind == LayoutKindSection.
	SectionID   string `json:"sectionId"`
	SectionName string `json:"sectionName"`
}

// NamedLayout is one entry in State.MixerLayouts — a named, independently
// ordered mixer layout an aux can be assigned to (see Aux.LayoutID). The
// DefaultLayoutID entry always exists (see ensureDefaultLayout) and is the
// one every aux without an explicit LayoutID renders.
type NamedLayout struct {
	Name  string       `json:"name"`
	Items []LayoutItem `json:"items"`
}

// State mirrors config/state.json — the admin-editable overlay merged onto
// live desk data by hub.buildConfig().
type State struct {
	UUID         string                 `json:"uuid"`
	Name         string                 `json:"name"`
	SessionName  string                 `json:"sessionName"`
	Auxes        []Aux                  `json:"auxes"`
	MixerLayouts map[string]NamedLayout `json:"mixerLayouts"`
}

// Aux is the admin-editable overlay for one aux bus, indexed 1:1 by
// position (aux N is Auxes[N-1]) since auxes stay the fixed destination
// concept — unlike sources/sections, which are reorderable/interleavable
// (see LayoutItem).
type Aux struct {
	Enabled bool   `json:"enabled"`
	Colour  string `json:"colour"`
	Icon    string `json:"icon"`

	// LayoutID names the entry in State.MixerLayouts this aux renders in
	// the mixer client. Empty, or any id no longer present in
	// MixerLayouts (e.g. a since-deleted custom layout), resolves to
	// DefaultLayoutID — deleting a layout never blocks on or warns about
	// auxes still pointing at it, they just silently fall back (see the
	// design doc's "delete-in-use" decision).
	LayoutID string `json:"layoutId"`
}
```

Leave `Preset` unchanged (it embeds `State`, so it picks up `MixerLayouts` automatically).

- [ ] **Step 2: Update `config.go` — `ensureDefaultLayout`, `NewDefaultState`, the legacy shim in `LoadStateOrDefault`/`LoadPresets`, and `UpdatePreset`'s new signature**

In `internal/config/config.go`, add near the top (after the `Manager` type, before `LoadGlobalOrDefault`):

```go
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
```

Replace `LoadStateOrDefault`:

```go
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
```

Replace `LoadPresets` (now parses via `[]json.RawMessage` first so the same per-entry legacy shim can inspect each preset's original bytes):

```go
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
```

Replace `UpdatePreset`'s signature and body:

```go
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
```

- [ ] **Step 3: Rewrite `config_test.go`'s existing `MixerLayout`-touching tests**

Replace `TestSaveAndLoadStateRoundTrip`:

```go
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
```

Replace `TestPresetLifecycle`'s `state` construction line:

```go
	state := State{MixerLayouts: map[string]NamedLayout{
		DefaultLayoutID: {Name: "Default", Items: []LayoutItem{{Kind: LayoutKindSource, Type: "channel", Number: 1, Enabled: true}}},
	}}
```

(the rest of `TestPresetLifecycle` is unchanged — it never reads `MixerLayout` again after that line).

Replace `TestUpdatePreset`:

```go
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
```

- [ ] **Step 4: Add new tests for the default-layout invariant, the legacy shim, and multi-layout round trips**

Append to `config_test.go`:

```go
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
```

Add `"os"` and `"path/filepath"` to `config_test.go`'s import block if not already present (check the existing imports — currently `errors`, `testing`; both new imports are needed for the two shim tests above).

- [ ] **Step 5: Run the config package tests**

Run: `cd internal/config && go test ./... -run . -v`
Expected: PASS for all tests, including the 4 new ones.

- [ ] **Step 6: Commit**

```bash
git add internal/config/types.go internal/config/config.go internal/config/config_test.go
git commit -m "$(cat <<'EOF'
Rename config.State.MixerLayout to MixerLayouts, add aux-specific layouts

State.MixerLayout []LayoutItem becomes MixerLayouts map[string]NamedLayout
keyed by a stable id, with "default" always present. Aux gains LayoutID.
Includes a one-time compat shim for old state.json/presets.json files
carrying the singular "mixerLayout" key.
EOF
)"
```

---

## Task 2: Wire protocol — `oscmsg.ConfigPayload.MixerLayouts`, `AuxConfig.LayoutID`

**Files:**
- Modify: `internal/oscmsg/config.go`
- Modify: `internal/oscmsg/message_test.go`

**Interfaces:**
- Consumes: nothing from Task 1 (oscmsg has no dependency on the config package — it's the wire-facing mirror, per the existing `LayoutKindSource`/`LayoutKindSection` duplication convention).
- Produces: `oscmsg.NamedLayout{Name string, Items []LayoutItem}`, `oscmsg.ConfigPayload.MixerLayouts map[string]NamedLayout`, `oscmsg.AuxConfig.LayoutID string`, `oscmsg.DefaultLayoutID = "default"`.

- [ ] **Step 1: Rewrite `internal/oscmsg/config.go`**

```go
package oscmsg

// LayoutKindSource/LayoutKindSection are LayoutItem.Kind's two values —
// mirrors config.LayoutKindSource/LayoutKindSection, kept as separate
// constants since this is the wire-facing type, not the persisted one.
const (
	LayoutKindSource  = "source"
	LayoutKindSection = "section"
)

// DefaultLayoutID mirrors config.DefaultLayoutID — see its doc comment.
const DefaultLayoutID = "default"

// LayoutItem is one entry in a NamedLayout's Items — the single ordered
// sequence a mixer/admin client renders and drags, mixing sources
// (channels and groups — see the plan's "Group buses" section, both sent
// to auxes via the same Aux_Send/N/send_level|send_pan address shape,
// just under different namespace roots) and admin-created section
// markers. See config.LayoutItem's doc comment for why array position —
// not a stored order/section field — is the single source of truth for
// both display order and a source's section membership.
type LayoutItem struct {
	Kind string `json:"kind"` // LayoutKindSource | LayoutKindSection

	// Set when Kind == LayoutKindSource.
	Type    string `json:"type"` // "channel" | "group"
	Number  int    `json:"number"`
	Label   string `json:"label"`
	Enabled bool   `json:"enabled"`
	Stereo  bool   `json:"stereo"`
	Icon    string `json:"icon"`

	// Set when Kind == LayoutKindSection.
	SectionID   string `json:"sectionId"`
	SectionName string `json:"sectionName"`
}

// NamedLayout is one entry in ConfigPayload.MixerLayouts — mirrors
// config.NamedLayout, duplicated here since this is the wire-facing
// shape, not the persisted one (same reasoning as LayoutItem/LayoutKind
// above). Name rides along even though the mixer client only ever looks a
// layout up by its map key and ignores it: ConfigPayload doubles as
// internal/admin's AdminQuery reply too (see hub.DeskStatusInfo's doc
// comment for the same "one payload, two consumers" shape), and the admin
// UI's layout picker/preset save both need the name without a second
// fetch path back to state.json.
type NamedLayout struct {
	Name  string       `json:"name"`
	Items []LayoutItem `json:"items"`
}

// ConfigPayload is what a mixer client needs to render itself: every
// named mixer layout (see NamedLayout — the client picks one by whichever
// aux is selected, see AuxConfig.LayoutID) and the list of destination
// aux buses.
type ConfigPayload struct {
	MixerLayouts map[string]NamedLayout `json:"mixerLayouts"`
	Auxes        []AuxConfig             `json:"auxes"`
	Snapshot     string                  `json:"snapshot"`
}

// AuxConfig describes one destination aux bus.
type AuxConfig struct {
	Channel int    `json:"channel"`
	Label   string `json:"label"`
	Enabled bool   `json:"enabled"`
	Stereo  bool   `json:"stereo"`
	Colour  string `json:"colour"`
	Icon    string `json:"icon"`

	// LayoutID names which entry in ConfigPayload.MixerLayouts this aux
	// renders — mirrors config.Aux.LayoutID's doc comment. Empty (or
	// naming an id no longer in MixerLayouts) means the mixer client
	// falls back to DefaultLayoutID — see mixer.js's layoutFor().
	LayoutID string `json:"layoutId"`
}
```

- [ ] **Step 2: Add a wire-shape round-trip test**

Append to `internal/oscmsg/message_test.go`:

```go
// TestConfigPayloadMixerLayoutsRoundTrip guards the wire shape itself:
// MixerLayouts must marshal as a JSON object keyed by layout id (not an
// array), each value carrying both name and items, and AuxConfig must
// carry layoutId — this is what mixer.js's onMessage/admin's queryConfig
// both parse.
func TestConfigPayloadMixerLayoutsRoundTrip(t *testing.T) {
	cfg := ConfigPayload{
		MixerLayouts: map[string]NamedLayout{
			DefaultLayoutID: {Name: "Default", Items: []LayoutItem{{Kind: LayoutKindSource, Type: "channel", Number: 1, Enabled: true}}},
			"mon-1":         {Name: "Monitor Mix", Items: []LayoutItem{{Kind: LayoutKindSection, SectionID: "s1", SectionName: "Drums"}}},
		},
		Auxes:    []AuxConfig{{Channel: 1, LayoutID: "mon-1"}},
		Snapshot: "Show A",
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !json.Valid(data) {
		t.Fatalf("marshaled output is not valid JSON: %s", data)
	}

	var got ConfigPayload
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.MixerLayouts) != 2 {
		t.Fatalf("MixerLayouts = %+v, want 2 entries", got.MixerLayouts)
	}
	if got.MixerLayouts["mon-1"].Name != "Monitor Mix" || len(got.MixerLayouts["mon-1"].Items) != 1 {
		t.Fatalf("mon-1 = %+v, want Name=Monitor Mix, 1 item", got.MixerLayouts["mon-1"])
	}
	if len(got.Auxes) != 1 || got.Auxes[0].LayoutID != "mon-1" {
		t.Fatalf("auxes = %+v, want aux 1 layoutId=mon-1", got.Auxes)
	}
}
```

- [ ] **Step 3: Run the oscmsg package tests**

Run: `cd internal/oscmsg && go test ./... -v`
Expected: PASS for all tests, including the new one.

- [ ] **Step 4: Commit**

```bash
git add internal/oscmsg/config.go internal/oscmsg/message_test.go
git commit -m "$(cat <<'EOF'
Add NamedLayout/LayoutID to the wire ConfigPayload

Mirrors config's mixerLayout -> mixerLayouts rename on the wire side:
ConfigPayload.MixerLayouts is now keyed by layout id with each entry
carrying Name+Items, and AuxConfig carries which layout it uses.
EOF
)"
```

---

## Task 3: Hub — per-layout `buildConfig`, `AdminEvent`/`Options` rename, `main.go` wiring

**Files:**
- Modify: `internal/hub/hub.go`
- Modify: `internal/hub/admin.go`
- Modify: `internal/hub/buildconfig.go`
- Modify: `internal/hub/hub_test.go`
- Modify: `cmd/oscwebmixer/main.go`

**Interfaces:**
- Consumes: `config.NamedLayout`, `config.DefaultLayoutID`, `config.Aux.LayoutID` (Task 1); `oscmsg.NamedLayout`, `oscmsg.ConfigPayload.MixerLayouts`, `oscmsg.AuxConfig.LayoutID` (Task 2).
- Produces: `Hub.layouts map[string]config.NamedLayout` (was `layout []config.LayoutItem`), `AdminEvent.Layouts map[string]config.NamedLayout` (was `Layout`), `Options.InitialLayouts map[string]config.NamedLayout` (was `InitialLayout`) — all consumed by Task 5/6's admin package.

- [ ] **Step 1: Rename the `layout` field and `Options`/`AdminEvent` in `hub.go`**

In `internal/hub/hub.go`, change the struct field and its doc comment (around line 183-193):

```go
	// layouts/auxOverrides are the admin-editable overlay loaded from
	// config/state.json — layered onto the live desk-reported data in
	// buildConfig(). layouts holds every named layout keyed by id (see
	// config.NamedLayout's doc comment for why array position within
	// each one is the single source of truth for both display order and
	// section membership). Updated live via AdminEvent, no restart
	// required (see Phase 3 scope note: only network settings need a
	// restart, everything here applies immediately and re-broadcasts to
	// connected clients).
	layouts      map[string]config.NamedLayout
	auxOverrides []config.Aux
```

Change `Options` (around line 283-288):

```go
	// InitialLayouts/InitialAuxes seed the admin overlay from
	// config/state.json at startup, so the very first buildConfig() call
	// already reflects persisted admin edits — not just messages arriving
	// after the first AdminEvent.
	InitialLayouts map[string]config.NamedLayout
	InitialAuxes   []config.Aux
```

Change `New()`'s struct literal (around line 333):

```go
		layouts:                 opts.InitialLayouts,
```

- [ ] **Step 2: Rename `AdminEvent.Layout` in `admin.go`**

In `internal/hub/admin.go`, change the field (around line 51):

```go
	// The following are set for AdminApply.
	Layouts                 map[string]config.NamedLayout
	Auxes                   []config.Aux
	ExternalDevices         []ExternalDevice
	AutoHideChannelPatterns []string
	AutoHideGroupPatterns   []string
	AutoHideAuxPatterns     []string
	Debug                   bool
```

Change `handleAdminEvent`'s `AdminApply` case (around line 84):

```go
	case AdminApply:
		h.layouts = ev.Layouts
		h.auxOverrides = ev.Auxes
```

The rest of that case (log line, broadcast loop) is unchanged.

- [ ] **Step 3: Rewrite `buildConfig()` in `buildconfig.go` to loop over every named layout**

Replace the whole function body from the `layout := make(...)` line to the end:

```go
func (h *Hub) buildConfig() oscmsg.ConfigPayload {
	// matchesAny is the fallback-only "should a not-yet-customized entry
	// start visible" check — see config.Global's AutoHide*Patterns doc
	// comment. Never consulted for an entry already present in
	// h.layouts/h.auxOverrides; those always keep their stored Enabled.
	// Patterns are checked per source type (not one shared list) because
	// names collide by convention across types — an aux bus "Hall" and its
	// return channel "Hall Ret" both contain "Hall", but only one should
	// actually be hidden.
	matchesAny := func(label string, patterns []string) bool {
		if label == "" {
			return false
		}
		lower := strings.ToLower(label)
		for _, p := range patterns {
			if p != "" && strings.Contains(lower, strings.ToLower(p)) {
				return true
			}
		}
		return false
	}

	var auxes []oscmsg.AuxConfig
	if modes, ok := h.cache["/Console/Aux_Outputs/modes"]; ok {
		auxes = make([]oscmsg.AuxConfig, 0, len(modes.Args))
		for i, m := range modes.Args {
			channel := i + 1
			label := ""
			if nameMsg, ok := h.cache[fmt.Sprintf("/Aux_Outputs/%d/Buss_Trim/name", channel)]; ok && len(nameMsg.Args) > 0 {
				label = argAsString(nameMsg.Args[0])
			}

			enabled := !matchesAny(label, h.autoHideAuxPatterns)
			colour := generateColour(len(modes.Args), i)
			icon := ""
			layoutID := ""
			if i < len(h.auxOverrides) {
				ov := h.auxOverrides[i]
				enabled = ov.Enabled
				if ov.Colour != "" {
					colour = ov.Colour
				}
				icon = ov.Icon
				layoutID = ov.LayoutID
			}

			auxes = append(auxes, oscmsg.AuxConfig{
				Channel:  channel,
				Label:    label,
				Enabled:  enabled,
				Stereo:   argAsInt(m) == 2,
				Colour:   colour,
				Icon:     icon,
				LayoutID: layoutID,
			})
		}
	}

	groupModes, haveGroupModes := h.cache["/Console/Group_Outputs/modes"]

	// liveSource looks up live desk-reported label/stereo for one
	// channel/group — shared by both the laid-out walk below and the
	// natural-order fallback for anything a layout doesn't mention.
	liveSource := func(sourceType string, number int) (label string, stereo bool) {
		if sourceType == "group" {
			if nameMsg, ok := h.cache[fmt.Sprintf("/Group_Outputs/%d/Buss_Trim/name", number)]; ok && len(nameMsg.Args) > 0 {
				label = argAsString(nameMsg.Args[0])
			}
			if haveGroupModes && number-1 < len(groupModes.Args) {
				stereo = argAsInt(groupModes.Args[number-1]) == 2
			}
			return
		}
		if nameMsg, ok := h.cache[fmt.Sprintf("/Input_Channels/%d/Channel_Input/name", number)]; ok && len(nameMsg.Args) > 0 {
			label = argAsString(nameMsg.Args[0])
		}
		if modeMsg, ok := h.cache[fmt.Sprintf("/Input_Channels/%d/Channel_Input/stereo_mode", number)]; ok && len(modeMsg.Args) > 0 {
			stereo = argAsInt(modeMsg.Args[0]) == 2
		}
		return
	}

	exists := func(sourceType string, number int) bool {
		if sourceType == "group" {
			return number >= 1 && number <= h.groupCount
		}
		return number >= 1 && number <= h.channelCount
	}

	// buildLayout runs the merge (live label/stereo overlay, natural-
	// order fallback for anything items doesn't mention) for one named
	// layout's Items — called once per entry in h.layouts below, so every
	// layout is independently sparse and gets the same fallback treatment
	// the single global layout got before named layouts existed.
	buildLayout := func(items []config.LayoutItem) []oscmsg.LayoutItem {
		layout := make([]oscmsg.LayoutItem, 0, len(items)+h.channelCount+h.groupCount)
		placed := make(map[string]bool, len(items))
		for _, item := range items {
			if item.Kind == config.LayoutKindSection {
				layout = append(layout, oscmsg.LayoutItem{
					Kind: oscmsg.LayoutKindSection, SectionID: item.SectionID, SectionName: item.SectionName,
				})
				continue
			}
			if !exists(item.Type, item.Number) {
				// Session changed to fewer channels/groups than this entry
				// references (confirmed live: sessions can differ in channel
				// count) — drop it rather than render a source that no longer
				// exists on the desk. It simply reappears if a future session
				// has that channel again and the admin re-adds it.
				continue
			}
			label, stereo := liveSource(item.Type, item.Number)
			layout = append(layout, oscmsg.LayoutItem{
				Kind: oscmsg.LayoutKindSource, Type: item.Type, Number: item.Number,
				Label: label, Enabled: item.Enabled, Stereo: stereo, Icon: item.Icon,
			})
			placed[sourceKey(item.Type, item.Number)] = true
		}
		for i := 1; i <= h.channelCount; i++ {
			if placed[sourceKey("channel", i)] {
				continue
			}
			label, stereo := liveSource("channel", i)
			layout = append(layout, oscmsg.LayoutItem{Kind: oscmsg.LayoutKindSource, Type: "channel", Number: i, Label: label, Enabled: !matchesAny(label, h.autoHideChannelPatterns), Stereo: stereo})
		}
		for i := 1; i <= h.groupCount; i++ {
			if placed[sourceKey("group", i)] {
				continue
			}
			label, stereo := liveSource("group", i)
			layout = append(layout, oscmsg.LayoutItem{Kind: oscmsg.LayoutKindSource, Type: "group", Number: i, Label: label, Enabled: !matchesAny(label, h.autoHideGroupPatterns), Stereo: stereo})
		}
		return layout
	}

	layouts := make(map[string]oscmsg.NamedLayout, len(h.layouts))
	for id, nl := range h.layouts {
		layouts[id] = oscmsg.NamedLayout{Name: nl.Name, Items: buildLayout(nl.Items)}
	}

	return oscmsg.ConfigPayload{
		MixerLayouts: layouts,
		Auxes:        auxes,
		Snapshot:     h.snapshotName,
	}
}
```

Also update the function's leading doc comment (currently says "the actual display sequence itself" for the singular `h.layout`) to reflect multiple layouts:

```go
// buildConfig assembles what a mixer client needs to render itself: live
// desk-reported data (names, aux stereo/mono, counts) as the base, with the
// admin-editable overlay (h.layouts/h.auxOverrides, from config/state.json)
// layered on top for enabled/icon/colour and — for each entry in h.layouts
// — the actual display sequence itself. A source not otherwise mentioned in
// a given layout defaults to enabled (unless its live name matches that
// type's auto-hide patterns — see matchesAny and config.Global's
// AutoHide*Patterns fields), no icon, and is appended at the end in
// natural desk-reported order (channels then groups) — see buildLayout
// below, run once per named layout.
```

- [ ] **Step 4: Update `main.go`'s `Options` construction**

In `cmd/oscwebmixer/main.go`, change (around line 160):

```go
		InitialLayouts:          state.MixerLayouts,
```

- [ ] **Step 5: Mechanically rename `.MixerLayout` field accesses in `hub_test.go`**

Run this from the repo root — it rewrites every `.MixerLayout` field access (not followed by `s`, so it never touches the new `MixerLayouts` name) to `.MixerLayouts["default"].Items`:

```bash
perl -pi -e 's/\.MixerLayout\b/.MixerLayouts["default"].Items/g' internal/hub/hub_test.go
```

This covers the two straightforward read-sites already found (`configEnv.Config.MixerLayout` at what was line 609/612/1265, `cfg.MixerLayout` at what was line 1594-1607/1664) — after this, run `gofmt -l internal/hub/hub_test.go` to confirm formatting is untouched (should print nothing) and `git diff internal/hub/hub_test.go` to eyeball that no doc-comment prose got corrupted (there is none in this file today — confirmed no `//....MixerLayout` matches before this rename — but a quick skim is still the cheap safety net).

- [ ] **Step 6: Manually fix the two `AdminEvent{Layout: ...}` struct literals the sed can't touch**

In `internal/hub/hub_test.go`, find `TestAdminApplyLayoutPreservesOrderAndAppendsUnmentioned` (around what was line 1551) and replace its `AdminEvent{...}` construction:

```go
	adminEvents <- AdminEvent{
		Kind: AdminApply,
		Layouts: map[string]config.NamedLayout{
			config.DefaultLayoutID: {
				Name: "Default",
				Items: []config.LayoutItem{
					{Kind: config.LayoutKindSource, Type: "channel", Number: 2, Enabled: true},
					{Kind: config.LayoutKindSection, SectionID: "b", SectionName: "Section B"},
					{Kind: config.LayoutKindSource, Type: "channel", Number: 3, Enabled: false},
				},
			},
		},
	}
```

And in `TestAutoHidePatternsHideUnmentionedMatches` (around what was line 1644):

```go
	adminEvents <- AdminEvent{
		Kind: AdminApply,
		Layouts: map[string]config.NamedLayout{
			config.DefaultLayoutID: {
				Name:  "Default",
				Items: []config.LayoutItem{{Kind: config.LayoutKindSource, Type: "group", Number: 1, Enabled: true}},
			},
		},
		AutoHideGroupPatterns: []string{"master"},
		AutoHideAuxPatterns:   []string{"hall"},
	}
```

Every other `for _, it := range cfg.MixerLayouts["default"].Items` loop in both these tests (post-rename from Step 5) stays as-is — it now iterates the same field, just one level deeper.

- [ ] **Step 7: Add a test proving multiple named layouts are built independently, and that `AuxConfig.LayoutID` passes through**

Append to `hub_test.go`:

```go
// TestBuildConfigPerLayoutIndependence proves buildConfig runs its merge
// (explicit items + natural-order fallback) once per entry in h.layouts,
// not just for "default" — two differently-ordered/customized layouts
// covering the same desk must each come back independently correct, and
// an aux's LayoutID must ride through to the wire AuxConfig unchanged.
func TestBuildConfigPerLayoutIndependence(t *testing.T) {
	fakeDesk := &scriptedDesk{
		inbound:      make(chan desk.InboundMessage, 64),
		channelCount: 2, auxCount: 1, groupCount: 0,
		channelNames: map[int]string{1: "One", 2: "Two"},
		auxNames:     map[int]string{1: "Mon 1"},
	}
	h := New(fakeDesk, nil, log.New(io.Discard, "", 0), Options{StartupRetryInterval: 50 * time.Millisecond})
	adminEvents := make(chan AdminEvent, 4)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go h.Run(ctx, fakeDesk.inbound, nil, adminEvents)

	select {
	case <-h.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("startup did not complete")
	}

	adminEvents <- AdminEvent{
		Kind: AdminApply,
		Layouts: map[string]config.NamedLayout{
			config.DefaultLayoutID: {
				Name:  "Default",
				Items: []config.LayoutItem{{Kind: config.LayoutKindSource, Type: "channel", Number: 1, Enabled: true}},
			},
			"mon-1": {
				Name:  "Monitor Mix",
				Items: []config.LayoutItem{{Kind: config.LayoutKindSource, Type: "channel", Number: 2, Enabled: false}},
			},
		},
		Auxes: []config.Aux{{Enabled: true, LayoutID: "mon-1"}},
	}

	reply := make(chan oscmsg.ConfigPayload, 1)
	adminEvents <- AdminEvent{Kind: AdminQuery, Reply: reply}

	var cfg oscmsg.ConfigPayload
	select {
	case cfg = <-reply:
	case <-time.After(time.Second):
		t.Fatal("did not receive AdminQuery reply")
	}

	if len(cfg.MixerLayouts) != 2 {
		t.Fatalf("MixerLayouts = %+v, want 2 entries", cfg.MixerLayouts)
	}

	def := cfg.MixerLayouts[config.DefaultLayoutID]
	if len(def.Items) != 2 || def.Items[0].Number != 1 || !def.Items[0].Enabled {
		t.Fatalf("default layout = %+v, want channel 1 enabled first", def.Items)
	}
	if def.Items[1].Number != 2 || !def.Items[1].Enabled {
		t.Fatalf("default layout = %+v, want channel 2 appended (unmentioned), enabled by default", def.Items)
	}

	mon := cfg.MixerLayouts["mon-1"]
	if mon.Name != "Monitor Mix" {
		t.Fatalf("mon-1 name = %q, want Monitor Mix", mon.Name)
	}
	if len(mon.Items) != 2 || mon.Items[0].Number != 2 || mon.Items[0].Enabled {
		t.Fatalf("mon-1 layout = %+v, want channel 2 disabled first (independent of default's order/enabled)", mon.Items)
	}
	if mon.Items[1].Number != 1 || !mon.Items[1].Enabled {
		t.Fatalf("mon-1 layout = %+v, want channel 1 appended (unmentioned in mon-1), enabled by default", mon.Items)
	}

	if len(cfg.Auxes) != 1 || cfg.Auxes[0].LayoutID != "mon-1" {
		t.Fatalf("auxes = %+v, want aux 1 layoutId=mon-1", cfg.Auxes)
	}
}
```

- [ ] **Step 8: Run the hub package tests**

Run: `cd internal/hub && go test ./... -v`
Expected: PASS for all tests, including the new one. If Step 5's `perl` rename missed anything (a compile error naming a file/line), fix that specific site by hand using the same `.MixerLayouts["default"].Items` shape and re-run.

- [ ] **Step 9: Build the whole module to catch any other break (e.g. `cmd/oscwebmixer`)**

Run: `go build ./...`
Expected: no errors.

- [ ] **Step 10: Commit**

```bash
git add internal/hub/hub.go internal/hub/admin.go internal/hub/buildconfig.go internal/hub/hub_test.go cmd/oscwebmixer/main.go
git commit -m "$(cat <<'EOF'
Make hub.buildConfig produce every named layout, not just one

Hub.layouts/AdminEvent.Layouts/Options.InitialLayouts all move from a
single []LayoutItem to map[string]config.NamedLayout. buildConfig's
merge (live overlay + natural-order fallback) now runs once per named
layout. AuxConfig.LayoutID rides through from config.Aux.LayoutID.
EOF
)"
```

---

## Task 4: Admin backend — make existing Sources/Auxes endpoints layout-aware

**Files:**
- Modify: `internal/admin/view.go`
- Modify: `internal/admin/render.go`
- Modify: `internal/admin/admin.go`
- Modify: `internal/admin/handlers.go`
- Modify: `internal/admin/admin_test.go`

**Interfaces:**
- Consumes: `oscmsg.NamedLayout`, `oscmsg.ConfigPayload.MixerLayouts`, `oscmsg.AuxConfig.LayoutID`, `oscmsg.DefaultLayoutID` (Task 2); `config.NamedLayout`, `config.Aux.LayoutID` (Task 1).
- Produces: `LayoutOption{ID, Name}`, `sourcesSectionData`/`auxesSectionData` package functions, `formLayoutID(*http.Request) string`, `(*Handler).saveLayout(ctx, layoutID string, items []config.LayoutItem, auxes []config.Aux)`, `(*Handler).saveAuxes(ctx, auxes []config.Aux)` — Task 5's new layout-management handlers and Task 6's preset handlers both call these.

- [ ] **Step 1: Add `LayoutOption` and rewrite `SourcesSectionData`/`AuxesSectionData`/`AuxRow` in `view.go`**

In `internal/admin/view.go`, add `"sort"` to the import block, then add near the top (after `ViewData`, before `NetworkSectionData`):

```go
// LayoutOption is one entry in a layout picker — the Sources tab's
// layout `<select>` and the Auxes tab's per-aux layout `<select>` both
// render from the same list (see layoutOptions).
type LayoutOption struct {
	ID   string
	Name string
}
```

Replace `SourcesSectionData`:

```go
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
	Layouts    []LayoutOption
	OOB        bool
}
```

Replace `AuxesSectionData`:

```go
type AuxesSectionData struct {
	Auxes   []AuxRow
	Icons   []string // available aux icon picker options — see Handler.icons
	Layouts []LayoutOption
	OOB     bool
}
```

Replace `AuxRow`:

```go
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
```

- [ ] **Step 2: Rewrite `toSourcesSectionData`/`toAuxRows`/`configAuxesFromPayload`, add `layoutOptions`/`sourcesSectionData`/`auxesSectionData`**

Replace `toSourcesSectionData`:

```go
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
	return SourcesSectionData{Items: rows, LayoutID: layoutID, LayoutName: layoutName, Layouts: layouts, OOB: oob}
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
	sort.Slice(opts, func(i, j int) bool { return opts[i].Name < opts[j].Name })
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
```

Replace `toAuxRows`:

```go
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
```

Replace `configAuxesFromPayload`:

```go
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
```

`layoutFromPayload` (converts `[]oscmsg.LayoutItem` → `[]config.LayoutItem`) is unchanged — it already operates per-items-slice, not on the whole payload.

- [ ] **Step 3: Rewrite `render.go`'s `renderSourcesSection`/`renderAuxesSection`**

```go
func (h *Handler) renderSourcesSection(w io.Writer, cfg oscmsg.ConfigPayload, layoutID string, oob bool) error {
	return h.tmpl.ExecuteTemplate(w, "sources-section", sourcesSectionData(cfg, layoutID, oob))
}

func (h *Handler) renderAuxesSection(w io.Writer, cfg oscmsg.ConfigPayload, oob bool) error {
	return h.tmpl.ExecuteTemplate(w, "auxes-section", auxesSectionData(cfg, h.icons, oob))
}
```

(`renderPresetsSection` is unchanged.)

- [ ] **Step 4: Split `admin.go`'s `saveState` into `saveLayout`/`saveAuxes`**

In `internal/admin/admin.go`, replace `saveState` and `applyState`'s `Layout:`/`Auxes:` construction:

```go
// saveLayout persists one named layout's items (layoutID) plus the auxes
// overlay in one atomic write, applies it live to hub, and returns a
// fresh post-apply ConfigPayload to render from — used by every
// Sources/Sections mutation. Every other layout in state.MixerLayouts is
// left untouched.
func (h *Handler) saveLayout(ctx context.Context, layoutID string, items []config.LayoutItem, auxes []config.Aux) (oscmsg.ConfigPayload, error) {
	state, err := h.cfg.LoadStateOrDefault()
	if err != nil {
		return oscmsg.ConfigPayload{}, err
	}
	nl := state.MixerLayouts[layoutID]
	nl.Items = items
	state.MixerLayouts[layoutID] = nl
	state.Auxes = auxes
	if err := h.cfg.SaveState(state); err != nil {
		return oscmsg.ConfigPayload{}, err
	}
	h.applyState(state)
	return h.queryConfig(ctx)
}

// saveAuxes persists the auxes overlay only, leaving every layout
// untouched — the Auxes tab's counterpart to saveLayout.
func (h *Handler) saveAuxes(ctx context.Context, auxes []config.Aux) (oscmsg.ConfigPayload, error) {
	state, err := h.cfg.LoadStateOrDefault()
	if err != nil {
		return oscmsg.ConfigPayload{}, err
	}
	state.Auxes = auxes
	if err := h.cfg.SaveState(state); err != nil {
		return oscmsg.ConfigPayload{}, err
	}
	h.applyState(state)
	return h.queryConfig(ctx)
}
```

In `applyState`, change the `AdminEvent{...}` construction's field name:

```go
	select {
	case h.adminEvents <- hub.AdminEvent{
		Kind: hub.AdminApply, Layouts: state.MixerLayouts, Auxes: state.Auxes,
		ExternalDevices:         devices,
		AutoHideChannelPatterns: autoHideChannel,
		AutoHideGroupPatterns:   autoHideGroup,
		AutoHideAuxPatterns:     autoHideAux,
		Debug:                   debug,
	}:
```

- [ ] **Step 5: Add `formLayoutID` and rewrite `handleIndex`/`handleSourcesSave`/`handleSourcesReset`/`handleSectionAdd`/`handleSectionDelete`/`handleAuxesSave` in `handlers.go`**

Add near the top of `handlers.go` (after imports, before `handleIndex`):

```go
// formLayoutID reads the posted layoutId field, defaulting to the Default
// layout — every Sources-tab request that doesn't explicitly name a
// layout (or names an empty one) targets Default, matching the picker's
// own "Default" option carrying id "default". Requires r.ParseForm to
// have already been called.
func formLayoutID(r *http.Request) string {
	if id := r.FormValue("layoutId"); id != "" {
		return id
	}
	return oscmsg.DefaultLayoutID
}
```

Replace `handleIndex`'s `data := ViewData{...}` construction:

```go
	data := ViewData{
		Global:      global,
		Network:     NetworkSectionData{Global: global, DeskStatus: status},
		SourcesData: sourcesSectionData(cfg, oscmsg.DefaultLayoutID, false),
		AuxesData:   auxesSectionData(cfg, h.icons, false),
		Presets:     toPresetRows(presets, state.UUID),
		Message:     r.URL.Query().Get("msg"),
		Version:     h.version,
	}
```

(A fresh page load always starts the Sources tab on Default — switching layouts mid-session is tracked client-side via the hidden `layoutId` field added in Task 6, not across full reloads.)

Replace `handleSourcesSave`:

```go
func (h *Handler) handleSourcesSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var wireLayout []oscmsg.LayoutItem
	if err := json.Unmarshal([]byte(r.FormValue("layout")), &wireLayout); err != nil {
		http.Error(w, "invalid layout: "+err.Error(), http.StatusBadRequest)
		return
	}
	layoutID := formLayoutID(r)

	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	cfg, err := h.queryConfig(ctx)
	if err != nil {
		http.Error(w, "hub not responding: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	fresh, err := h.saveLayout(ctx, layoutID, layoutFromPayload(wireLayout), configAuxesFromPayload(cfg.Auxes))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderSourcesSection(w, fresh, layoutID, false); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}
```

Replace `handleSourcesReset`:

```go
func (h *Handler) handleSourcesReset(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	layoutID := formLayoutID(r)

	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	cfg, err := h.queryConfig(ctx)
	if err != nil {
		http.Error(w, "hub not responding: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	fresh, err := h.saveLayout(ctx, layoutID, nil, configAuxesFromPayload(cfg.Auxes))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderSourcesSection(w, fresh, layoutID, false); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}
```

Replace `handleSectionAdd`'s body from `cfg, err := h.queryConfig(ctx)` onward:

```go
	layoutID := formLayoutID(r)

	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	cfg, err := h.queryConfig(ctx)
	if err != nil {
		http.Error(w, "hub not responding: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	id, err := config.NewID()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	newItem := config.LayoutItem{Kind: config.LayoutKindSection, SectionID: id, SectionName: name}

	layout := layoutFromPayload(cfg.MixerLayouts[layoutID].Items)
	insertAt := len(layout)
	if beforeKey := r.FormValue("beforeKey"); beforeKey != "" {
		for i, it := range layout {
			if layoutItemKey(it) == beforeKey {
				insertAt = i
				break
			}
		}
	}
	inserted := make([]config.LayoutItem, 0, len(layout)+1)
	inserted = append(inserted, layout[:insertAt]...)
	inserted = append(inserted, newItem)
	inserted = append(inserted, layout[insertAt:]...)

	fresh, err := h.saveLayout(ctx, layoutID, inserted, configAuxesFromPayload(cfg.Auxes))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderSourcesSection(w, fresh, layoutID, false); err != nil {
		h.log.Printf("admin render: %v", err)
	}
```

(the `name := strings.TrimSpace(r.FormValue("name"))` empty-name guard above it, and the `r.ParseForm()` call at the very top, are unchanged — `layoutID := formLayoutID(r)` just needs to come after `ParseForm` succeeds, before the empty-name early return is fine either way since `formLayoutID` doesn't error.)

Replace `handleSectionDelete`:

```go
func (h *Handler) handleSectionDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	layoutID := formLayoutID(r)

	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	cfg, err := h.queryConfig(ctx)
	if err != nil {
		http.Error(w, "hub not responding: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	layout := layoutFromPayload(cfg.MixerLayouts[layoutID].Items)
	filtered := layout[:0]
	for _, it := range layout {
		if it.Kind == config.LayoutKindSection && it.SectionID == id {
			continue
		}
		filtered = append(filtered, it)
	}

	fresh, err := h.saveLayout(ctx, layoutID, filtered, configAuxesFromPayload(cfg.Auxes))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderSourcesSection(w, fresh, layoutID, false); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}
```

Replace `handleAuxesSave`:

```go
func (h *Handler) handleAuxesSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	cfg, err := h.queryConfig(ctx)
	if err != nil {
		http.Error(w, "hub not responding: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	auxes := configAuxesFromPayload(cfg.Auxes)
	for i := range auxes {
		n := i + 1
		auxes[i].Enabled = r.Form.Has(fmt.Sprintf("enabled[%d]", n))
		auxes[i].Colour = r.FormValue(fmt.Sprintf("colour[%d]", n))
		auxes[i].Icon = r.FormValue(fmt.Sprintf("icon[%d]", n))
		auxes[i].LayoutID = r.FormValue(fmt.Sprintf("layout[%d]", n))
	}

	fresh, err := h.saveAuxes(ctx, auxes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderAuxesSection(w, fresh, false); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}
```

- [ ] **Step 6: Mechanically rename `.MixerLayout` field accesses in `admin_test.go`**

```bash
perl -pi -e 's/\.MixerLayout\b/.MixerLayouts["default"].Items/g' internal/admin/admin_test.go
gofmt -l internal/admin/admin_test.go
git diff internal/admin/admin_test.go | less
```

`gofmt -l` should print nothing; skim the diff for anything that isn't a straightforward `.MixerLayouts["default"].Items` substitution (there's no comment-prose collision in this file today, confirmed during planning, but the skim is the cheap safety net). This rename covers `TestSectionCreateAppearsInResponse`, `TestSectionAddInsertsAtGivenPosition`, `TestSectionAddFallsBackToAppendOnUnmatchedBeforeKey`, `TestSourcesSavePreservesOrderAndSectionPlacement`, `TestSectionDeleteKeepsSourcesRemovesMarker`, `TestSourcesReset`, `TestSourcesSaveRoundTrip`, `TestPresetsReset`, `TestPresetActiveAndUpdate` (including its `updated.MixerLayout` preset-side access), `TestSettingsSaveAppliesLive`.

- [ ] **Step 7: Run the admin package tests, fix any remaining compile errors**

Run: `cd internal/admin && go test ./... -v`

The rename in Step 6 does NOT touch `postSourcesSave` (it never referenced `.MixerLayout`, it posts a JSON body) — it should keep working unmodified, since `formLayoutID` defaults to `"default"` when no `layoutId` field is posted, exactly matching every existing test's behavior. If a test fails to compile, it means Step 6's `perl` regex missed a construction-style site (a `MixerLayout:` colon-form struct literal) — grep `git diff --stat` for the file, find the failing line, and fix it by hand to whatever the equivalent `MixerLayouts["default"].Items` shape needs to be at that exact spot.

Expected: PASS for every existing test, no behavior change (they never set `layoutId`, so everything still targets `"default"`).

- [ ] **Step 8: Commit**

```bash
git add internal/admin/view.go internal/admin/render.go internal/admin/admin.go internal/admin/handlers.go internal/admin/admin_test.go
git commit -m "$(cat <<'EOF'
Make existing Sources/Auxes admin endpoints layout-aware

handleSourcesSave/Reset/handleSectionAdd/Delete now read a layoutId form
field (defaulting to "default") and mutate only that layout's items via
the new saveLayout, leaving other layouts untouched. handleAuxesSave
saves a per-aux layout[N] selection via the new saveAuxes. No new
endpoints or UI yet — every existing test still targets "default"
implicitly, so behavior is unchanged.
EOF
)"
```

---

## Task 5: Admin backend — layout management endpoints (create/rename/delete/switch) + presets

**Files:**
- Modify: `internal/admin/handlers.go`
- Modify: `internal/admin/admin.go`
- Modify: `internal/admin/admin_test.go`

**Interfaces:**
- Consumes: `formLayoutID`, `sourcesSectionData`, `layoutOptions`, `h.saveLayout`/`h.saveAuxes` (Task 4).
- Produces: routes `GET /admin/sources`, `POST /admin/layouts`, `POST /admin/layouts/{id}/rename`, `POST /admin/layouts/{id}/delete` — consumed by Task 6's templates.

- [ ] **Step 1: Add the four new routes in `admin.go`'s `Register`**

In `internal/admin/admin.go`, add inside `Register` (near the existing Sources routes):

```go
	mux.HandleFunc("GET /admin/sources", noStore(h.handleSourcesView))
	mux.HandleFunc("POST /admin/layouts", noStore(h.handleLayoutCreate))
	mux.HandleFunc("POST /admin/layouts/{id}/rename", noStore(h.handleLayoutRename))
	mux.HandleFunc("POST /admin/layouts/{id}/delete", noStore(h.handleLayoutDelete))
```

- [ ] **Step 2: Add `handleSourcesView`, `handleLayoutCreate`, `handleLayoutRename`, `handleLayoutDelete` to `handlers.go`**

Add after `handleSourcesReset`:

```go
// handleSourcesView switches which layout the Sources tab is viewing/
// editing — a plain GET (no mutation), re-rendering the section scoped to
// whichever layout id was requested. A missing id falls back to Default,
// same as every other "layoutId defaults to Default" spot in this
// package (see formLayoutID) — an id that no longer exists in
// MixerLayouts (e.g. a stale link to a since-deleted layout) renders an
// empty list rather than erroring, since the picker itself only ever
// offers ids that currently exist.
func (h *Handler) handleSourcesView(w http.ResponseWriter, r *http.Request) {
	layoutID := r.URL.Query().Get("layout")
	if layoutID == "" {
		layoutID = oscmsg.DefaultLayoutID
	}

	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	cfg, err := h.queryConfig(ctx)
	if err != nil {
		http.Error(w, "hub not responding: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderSourcesSection(w, cfg, layoutID, false); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}

// handleLayoutCreate adds a new named layout, cloning the layout named by
// the posted layoutId (the Sources tab's currently-viewed layout — see
// sources.html.tmpl's new-layout form) as a starting point, then switches
// the Sources tab to it.
func (h *Handler) handleLayoutCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `<p class="flash error">Layout name is required.</p>`)
		return
	}
	fromID := formLayoutID(r)

	id, err := config.NewID()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	state, err := h.cfg.LoadStateOrDefault()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	source := state.MixerLayouts[fromID]
	state.MixerLayouts[id] = config.NamedLayout{
		Name:  name,
		Items: append([]config.LayoutItem(nil), source.Items...),
	}
	if err := h.cfg.SaveState(state); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.applyState(state)

	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	cfg, err := h.queryConfig(ctx)
	if err != nil {
		http.Error(w, "hub not responding: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderSourcesSection(w, cfg, id, false); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}

// handleLayoutRename renames one custom layout — refused for "default",
// which always displays as "Default" (see config.DefaultLayoutID's doc
// comment).
func (h *Handler) handleLayoutRename(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == oscmsg.DefaultLayoutID {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `<p class="flash error">The Default layout can't be renamed.</p>`)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `<p class="flash error">Layout name is required.</p>`)
		return
	}

	state, err := h.cfg.LoadStateOrDefault()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	nl, ok := state.MixerLayouts[id]
	if !ok {
		http.NotFound(w, r)
		return
	}
	nl.Name = name
	state.MixerLayouts[id] = nl
	if err := h.cfg.SaveState(state); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.applyState(state)

	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	cfg, err := h.queryConfig(ctx)
	if err != nil {
		http.Error(w, "hub not responding: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderSourcesSection(w, cfg, id, false); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}

// handleLayoutDelete removes one custom layout — refused for "default".
// Any aux whose LayoutID pointed at the deleted layout is left as-is
// (still storing the now-dangling id): buildConfig's map lookup simply
// misses and falls back to Default for that aux, same as an aux that
// never had a custom layout (see the design doc's "delete-in-use" note —
// no usage check, no warning). Switches the Sources tab back to Default,
// since the layout it was viewing no longer exists.
func (h *Handler) handleLayoutDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == oscmsg.DefaultLayoutID {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `<p class="flash error">The Default layout can't be deleted.</p>`)
		return
	}

	state, err := h.cfg.LoadStateOrDefault()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	delete(state.MixerLayouts, id)
	if err := h.cfg.SaveState(state); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.applyState(state)

	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	cfg, err := h.queryConfig(ctx)
	if err != nil {
		http.Error(w, "hub not responding: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderSourcesSection(w, cfg, oscmsg.DefaultLayoutID, false); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}
```

- [ ] **Step 3: Update the four preset handlers to carry every layout, not just one**

Add this helper near `layoutFromPayload`/`configAuxesFromPayload` in `view.go` (it's the one-level-up mirror of `layoutFromPayload`):

```go
// configLayoutsFromPayload mirrors layoutFromPayload one level up — the
// wire NamedLayout shape (used for both the mixer client push and the
// admin's own queryConfig) to what's persisted. layoutFromPayload already
// strips the live-only Label/Stereo fields per item.
func configLayoutsFromPayload(layouts map[string]oscmsg.NamedLayout) map[string]config.NamedLayout {
	out := make(map[string]config.NamedLayout, len(layouts))
	for id, nl := range layouts {
		out[id] = config.NamedLayout{Name: nl.Name, Items: layoutFromPayload(nl.Items)}
	}
	return out
}
```

In `handlers.go`, replace `handlePresetSave`'s `state := config.State{...}` construction:

```go
	state := config.State{
		MixerLayouts: configLayoutsFromPayload(cfg.MixerLayouts),
		Auxes:        configAuxesFromPayload(cfg.Auxes),
	}
```

Replace `handlePresetUpdate`'s `h.cfg.UpdatePreset(...)` call:

```go
	_, err = h.cfg.UpdatePreset(uuid, configLayoutsFromPayload(cfg.MixerLayouts), configAuxesFromPayload(cfg.Auxes))
```

In `handlePresetLoad`, replace the two render calls at the end:

```go
	if err == nil {
		if err := h.renderSourcesSection(w, cfg, oscmsg.DefaultLayoutID, true); err != nil {
			h.log.Printf("admin render: %v", err)
		}
		if err := h.renderAuxesSection(w, cfg, true); err != nil {
			h.log.Printf("admin render: %v", err)
		}
	}
```

In `handlePresetsReset`, replace `def := config.State{Name: "Default"}` with `def := config.NewDefaultState()`, and replace the same two render calls at the end exactly as in `handlePresetLoad` above.

- [ ] **Step 4: Add tests for layout create/rename/delete/switch and multi-layout presets**

Append to `admin_test.go`:

```go
// TestLayoutCreateClonesAndSwitches proves + New layout clones the
// from-layout's items (not empty) and the response lands on the new
// layout (its picker option selected, its items shown).
func TestLayoutCreateClonesAndSwitches(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	postSourcesSave(t, mux, []oscmsg.LayoutItem{
		{Kind: oscmsg.LayoutKindSource, Type: "channel", Number: 1, Enabled: false, Icon: "kick.svg"},
	})

	rec := postForm(t, mux, "/admin/layouts", "name="+url.QueryEscape("Monitor Mix")+"&layoutId=default")
	body := rec.Body.String()
	if !strings.Contains(body, "Monitor Mix") {
		t.Fatalf("response does not mention the new layout name: %s", body)
	}

	cfg, err := h.queryConfig(context.Background())
	if err != nil {
		t.Fatalf("queryConfig: %v", err)
	}
	if len(cfg.MixerLayouts) != 2 {
		t.Fatalf("MixerLayouts = %+v, want 2 (default + Monitor Mix)", cfg.MixerLayouts)
	}
	var newID string
	for id, nl := range cfg.MixerLayouts {
		if nl.Name == "Monitor Mix" {
			newID = id
		}
	}
	if newID == "" {
		t.Fatal("no layout named Monitor Mix found")
	}
	items := cfg.MixerLayouts[newID].Items
	if len(items) != 1 || items[0].Number != 1 || items[0].Enabled || items[0].Icon != "kick.svg" {
		t.Fatalf("cloned items = %+v, want default's channel 1 (disabled, kick.svg icon) carried over", items)
	}
}

// TestLayoutRenameAndDeleteDefaultRefused proves the Default layout can't
// be renamed or deleted, but a custom one can be both.
func TestLayoutRenameAndDeleteDefaultRefused(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	rec := postForm(t, mux, "/admin/layouts/default/rename", "name=Nope")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("rename default: status = %d, want 400", rec.Code)
	}
	rec = postForm(t, mux, "/admin/layouts/default/delete", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("delete default: status = %d, want 400", rec.Code)
	}

	postForm(t, mux, "/admin/layouts", "name="+url.QueryEscape("Monitor Mix")+"&layoutId=default")
	cfg, err := h.queryConfig(context.Background())
	if err != nil {
		t.Fatalf("queryConfig: %v", err)
	}
	var id string
	for layoutID, nl := range cfg.MixerLayouts {
		if nl.Name == "Monitor Mix" {
			id = layoutID
		}
	}
	if id == "" {
		t.Fatal("Monitor Mix layout not found after create")
	}

	rec = postForm(t, mux, "/admin/layouts/"+id+"/rename", "name="+url.QueryEscape("Renamed"))
	if rec.Code != http.StatusOK {
		t.Fatalf("rename custom layout: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	cfg, _ = h.queryConfig(context.Background())
	if cfg.MixerLayouts[id].Name != "Renamed" {
		t.Fatalf("layout name = %q, want Renamed", cfg.MixerLayouts[id].Name)
	}

	rec = postForm(t, mux, "/admin/layouts/"+id+"/delete", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete custom layout: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	cfg, _ = h.queryConfig(context.Background())
	if _, ok := cfg.MixerLayouts[id]; ok {
		t.Fatalf("MixerLayouts = %+v, want %q gone after delete", cfg.MixerLayouts, id)
	}
}

// TestAuxLayoutFallsBackAfterDelete proves deleting a layout an aux
// points at doesn't block, warn, or error — the aux's LayoutID is left
// dangling and simply falls back to Default at the wire level (buildConfig
// never resolves it — this is purely a "does the map still have the key"
// lookup at render time, exercised here via ConfigPayload directly).
func TestAuxLayoutFallsBackAfterDelete(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	postForm(t, mux, "/admin/layouts", "name="+url.QueryEscape("Monitor Mix")+"&layoutId=default")
	cfg, err := h.queryConfig(context.Background())
	if err != nil {
		t.Fatalf("queryConfig: %v", err)
	}
	var id string
	for layoutID, nl := range cfg.MixerLayouts {
		if nl.Name == "Monitor Mix" {
			id = layoutID
		}
	}

	postForm(t, mux, "/admin/auxes", "layout[1]="+id)
	cfg, _ = h.queryConfig(context.Background())
	if cfg.Auxes[0].LayoutID != id {
		t.Fatalf("aux LayoutID = %q, want %q before delete", cfg.Auxes[0].LayoutID, id)
	}

	postForm(t, mux, "/admin/layouts/"+id+"/delete", "")
	cfg, _ = h.queryConfig(context.Background())
	if cfg.Auxes[0].LayoutID != id {
		t.Fatalf("aux LayoutID = %q, want it to still (harmlessly) name the deleted id %q — no rewrite-on-delete", cfg.Auxes[0].LayoutID, id)
	}
	if _, ok := cfg.MixerLayouts[cfg.Auxes[0].LayoutID]; ok {
		t.Fatalf("MixerLayouts still has %q, want it gone (deleted)", cfg.Auxes[0].LayoutID)
	}
	// The mixer client, not the server, is what resolves a dangling
	// LayoutID to Default (see mixer.js's layoutFor) — this test only
	// proves the server doesn't crash/block/rewrite anything on delete.
}

// TestPresetSaveLoadPreservesMultipleLayouts proves a preset snapshots
// every named layout, not just Default, and loading it restores all of
// them plus which layout each aux used.
func TestPresetSaveLoadPreservesMultipleLayouts(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	postForm(t, mux, "/admin/layouts", "name="+url.QueryEscape("Monitor Mix")+"&layoutId=default")
	cfg, err := h.queryConfig(context.Background())
	if err != nil {
		t.Fatalf("queryConfig: %v", err)
	}
	var monID string
	for id, nl := range cfg.MixerLayouts {
		if nl.Name == "Monitor Mix" {
			monID = id
		}
	}
	postSourcesSave(t, mux, []oscmsg.LayoutItem{
		{Kind: oscmsg.LayoutKindSource, Type: "channel", Number: 1, Enabled: false},
	})
	postForm(t, mux, "/admin/auxes", "layout[1]="+monID)

	postForm(t, mux, "/admin/presets/save", "name="+url.QueryEscape("Show 1"))
	presets, err := h.cfg.LoadPresets()
	if err != nil {
		t.Fatalf("LoadPresets: %v", err)
	}
	if len(presets) != 1 {
		t.Fatalf("presets = %+v, want 1", presets)
	}
	preset := presets[0]
	if len(preset.MixerLayouts) != 2 {
		t.Fatalf("preset.MixerLayouts = %+v, want 2 (default + Monitor Mix)", preset.MixerLayouts)
	}
	if preset.MixerLayouts[monID].Name != "Monitor Mix" {
		t.Fatalf("preset's monitor layout = %+v, want Name=Monitor Mix", preset.MixerLayouts[monID])
	}
	if preset.Auxes[0].LayoutID != monID {
		t.Fatalf("preset aux LayoutID = %q, want %q", preset.Auxes[0].LayoutID, monID)
	}

	// Reset, then load the preset back — everything should return.
	postForm(t, mux, "/admin/presets/reset", "")
	postForm(t, mux, "/admin/presets/"+preset.UUID+"/load", "")
	cfg, err = h.queryConfig(context.Background())
	if err != nil {
		t.Fatalf("queryConfig after load: %v", err)
	}
	if len(cfg.MixerLayouts) != 2 {
		t.Fatalf("MixerLayouts after load = %+v, want 2", cfg.MixerLayouts)
	}
	if cfg.Auxes[0].LayoutID != monID {
		t.Fatalf("aux LayoutID after load = %q, want %q", cfg.Auxes[0].LayoutID, monID)
	}
}
```

- [ ] **Step 5: Run the admin package tests**

Run: `cd internal/admin && go test ./... -v`
Expected: PASS for all tests, including the 5 new ones.

- [ ] **Step 6: Build the whole module**

Run: `go build ./...`
Expected: no errors.

- [ ] **Step 7: Commit**

```bash
git add internal/admin/handlers.go internal/admin/admin.go internal/admin/admin_test.go internal/admin/view.go
git commit -m "$(cat <<'EOF'
Add layout create/rename/delete/switch endpoints; presets carry every layout

GET /admin/sources?layout={id} switches which layout the Sources tab
views. POST /admin/layouts creates one (cloning the from-layout), with
rename/delete counterparts — both refused for "default". Deleting a
layout an aux points at doesn't block or rewrite anything: the aux's
LayoutID is left dangling and falls back to Default at render time.
Preset save/update/load/reset now round-trip every named layout, not
just one.
EOF
)"
```

---

## Task 6: Admin templates + `admin.js` — Sources layout picker, Auxes per-aux select

**Files:**
- Modify: `web/admin/sources.html.tmpl`
- Modify: `web/admin/auxes.html.tmpl`
- Modify: `web/admin/admin.js`
- Modify: `web/admin/admin.css`
- Modify: `internal/admin/admin_test.go`

**Interfaces:**
- Consumes: `SourcesSectionData{Items, LayoutID, LayoutName, Layouts, OOB}`, `AuxesSectionData{Auxes, Icons, Layouts, OOB}`, `AuxRow.LayoutID`, `LayoutOption{ID, Name}` (Task 4); routes from Task 5.

- [ ] **Step 1: Rewrite `sources.html.tmpl`'s `sources-section` block**

Replace the `{{define "sources-section"}}...{{end}}` block (leave `source-card`/`section-marker` untouched):

```html
{{define "sources-section"}}
<section id="section-sources"{{if .OOB}} hx-swap-oob="true"{{end}}>
	<h2>Sources (channels &amp; groups)</h2>
	<p class="hint">Sources are channels and group buses that can be mixed into auxes. Drag to reorder, hover over a source and click "+" to insert a section above.</p>

	<div class="layout-picker">
		<select name="layout" hx-get="/admin/sources" hx-target="#section-sources" hx-swap="outerHTML">
			{{range .Layouts}}
			<option value="{{.ID}}" {{if eq .ID $.LayoutID}}selected{{end}}>{{.Name}}</option>
			{{end}}
		</select>
		{{if ne .LayoutID "default"}}
		<input type="text" class="layout-name-field" name="name" value="{{.LayoutName}}" hx-post="/admin/layouts/{{.LayoutID}}/rename" hx-target="#section-sources" hx-swap="outerHTML" hx-trigger="change" />
		<button type="button" class="danger" hx-post="/admin/layouts/{{.LayoutID}}/delete" hx-target="#section-sources" hx-swap="outerHTML" hx-confirm="Delete this layout? Any aux using it falls back to Default.">Delete layout</button>
		{{end}}
	</div>

	<button
		type="button"
		class="danger"
		hx-post="/admin/sources/reset"
		hx-target="#section-sources"
		hx-swap="outerHTML"
		hx-include="[name='layoutId']"
		hx-confirm="Reset this layout to all channels then groups, ascending? This clears every section and customization."
	>Reset layout</button>
	<button type="button" id="sources-toggle-all">Show/Hide all</button>

	<form id="sources-form" hx-post="/admin/sources/save" hx-target="#section-sources" hx-swap="outerHTML">
		<input type="hidden" name="layoutId" value="{{.LayoutID}}" />
		<div id="sources-list">
			{{range .Items}}
				{{if .IsSection}}{{template "section-marker" .Section}}{{else}}{{template "source-card" .Source}}{{end}}
			{{end}}
		</div>
	</form>

	<form hx-post="/admin/sections" hx-target="#section-sources" hx-swap="outerHTML" hx-include="[name='layoutId']" class="inline-form">
		<input type="text" name="name" placeholder="New section name" required />
		<button type="submit">+ Add section</button>
	</form>

	<form hx-post="/admin/layouts" hx-target="#section-sources" hx-swap="outerHTML" hx-include="[name='layoutId']" class="inline-form">
		<input type="text" name="name" placeholder="New layout name" required />
		<button type="submit">+ New layout</button>
	</form>
</section>
{{end}}
```

(The hidden `layoutId` input lives inside `#sources-form`, which wraps `#sources-list` — every `source-card`/`section-marker` button that posts via that form's ancestry, including the per-row section-delete button, gets it included automatically by htmx's standard "closest enclosing form" behavior. The Reset button, "+ Add section" form, and "+ New layout" form are siblings of `#sources-form`, so they use `hx-include="[name='layoutId']"` to pull the same hidden field's value explicitly.)

- [ ] **Step 2: Add the per-aux layout `<select>` to `auxes.html.tmpl`**

Replace the `.aux-card` block's contents to add a layout select alongside the existing icon-picker:

```html
{{define "auxes-section"}}
<section id="section-auxes"{{if .OOB}} hx-swap-oob="true"{{end}}>
	<h2>Auxes</h2>
	<p class="hint">Auxes are the desk's mix buses - each one can become a personal mix a user can pick in the mixer app.</p>
	<button type="button" id="auxes-toggle-all">Show/Hide all</button>
	<form id="auxes-form" hx-post="/admin/auxes" hx-target="#section-auxes" hx-swap="outerHTML">
		<div class="cards">
			{{range .Auxes}}
			{{$aux := .}}
			<div class="aux-card{{if .Stereo}} stereo{{end}}">
				<span class="badge">Aux {{.Channel}}</span>
				<span class="label">{{.Label}}</span>
				<input type="checkbox" class="eye" title="Enabled" name="enabled[{{.Channel}}]" {{if .Enabled}}checked{{end}} />
				<input type="color" name="colour[{{.Channel}}]" value="{{.Colour}}" />
				<span class="icon-picker">
					<select name="icon[{{.Channel}}]" class="icon-select">
						<option value="">No icon</option>
						{{range $.Icons}}
						<option value="{{.}}" {{if eq . $aux.Icon}}selected{{end}}>{{iconLabel .}}</option>
						{{end}}
					</select>
					<img class="icon-preview" src="{{if .Icon}}/admin/icons/{{.Icon}}{{end}}" alt="" />
				</span>
				<select name="layout[{{.Channel}}]" class="layout-select" title="Mixer layout">
					{{range $.Layouts}}
					<option value="{{.ID}}" {{if eq .ID $aux.LayoutID}}selected{{end}}>{{.Name}}</option>
					{{end}}
				</select>
			</div>
			{{end}}
		</div>
	</form>
</section>
{{end}}
```

(No JS change needed for this select: the existing generic `"change"` listener in `admin.js` — `if (e.target.closest("#auxes-form")) { htmx.trigger(...) }` — already auto-submits `#auxes-form` on any change within it, including this new select.)

- [ ] **Step 3: Update the hover "+" section button in `admin.js` to carry `layoutId`**

In `web/admin/admin.js`, replace the click handler that calls `htmx.ajax("POST", "/admin/sections", ...)`:

```js
document.body.addEventListener("click", function (e) {
	const btn = e.target.closest(".add-section-btn");
	if (!btn) return;
	const layoutIdField = document.querySelector("#sources-form [name='layoutId']");
	htmx.ajax("POST", "/admin/sections", {
		target: "#section-sources",
		swap: "outerHTML",
		values: {
			name: "New section",
			beforeKey: btn.dataset.beforeKey || "",
			layoutId: layoutIdField ? layoutIdField.value : "",
		},
	});
});
```

- [ ] **Step 4: Add minimal layout for the new picker/rename controls in `admin.css`**

Add near `.icon-picker`'s rule:

```css
.layout-picker { display: flex; align-items: center; gap: 0.5rem; margin-bottom: 0.75rem; }
.layout-picker .layout-name-field { width: auto; min-width: 10rem; }
.icon-picker .layout-select { width: auto; min-width: 8rem; }
```

(Reuses the same generic `select`/`input[type="text"]`/`button` styles already in `admin.css` — no new component styling needed beyond alignment.)

- [ ] **Step 5: Add tests asserting the new markup renders**

Append to `admin_test.go`:

```go
// TestSourcesTabRendersLayoutPicker proves the Sources tab's layout
// picker/new-layout form render, and that a non-default layout shows
// rename/delete controls Default itself never shows.
func TestSourcesTabRendersLayoutPicker(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{`class="layout-picker"`, `name="layout"`, `+ New layout`} {
		if !strings.Contains(body, want) {
			t.Errorf("index page missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "Delete layout") {
		t.Fatal("Default layout view should not show Delete layout")
	}

	postForm(t, mux, "/admin/layouts", "name="+url.QueryEscape("Monitor Mix")+"&layoutId=default")
	cfg, err := h.queryConfig(context.Background())
	if err != nil {
		t.Fatalf("queryConfig: %v", err)
	}
	var id string
	for layoutID, nl := range cfg.MixerLayouts {
		if nl.Name == "Monitor Mix" {
			id = layoutID
		}
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin/sources?layout="+id, nil)
	mux.ServeHTTP(rec, req)
	body = rec.Body.String()
	for _, want := range []string{"Delete layout", `value="Monitor Mix"`} {
		if !strings.Contains(body, want) {
			t.Errorf("custom layout view missing %q: %s", want, body)
		}
	}
}

// TestAuxesTabRendersLayoutSelect proves each aux card's layout select
// lists every layout and preselects the aux's current LayoutID.
func TestAuxesTabRendersLayoutSelect(t *testing.T) {
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	postForm(t, mux, "/admin/layouts", "name="+url.QueryEscape("Monitor Mix")+"&layoutId=default")
	cfg, err := h.queryConfig(context.Background())
	if err != nil {
		t.Fatalf("queryConfig: %v", err)
	}
	var id string
	for layoutID, nl := range cfg.MixerLayouts {
		if nl.Name == "Monitor Mix" {
			id = layoutID
		}
	}
	postForm(t, mux, "/admin/auxes", "layout[1]="+id)

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, `name="layout[1]"`) {
		t.Fatalf("auxes tab missing layout select: %s", body)
	}
	if !strings.Contains(body, `value="`+id+`" selected`) && !strings.Contains(body, `value="`+id+`" selected`) {
		// tmpl attribute order: value before the conditional "selected"
		if !strings.Contains(body, id+`" selected`) {
			t.Errorf("aux 1's layout select doesn't preselect %q: %s", id, body)
		}
	}
}
```

- [ ] **Step 6: Run the admin package tests**

Run: `cd internal/admin && go test ./... -v`
Expected: PASS for all tests, including the 2 new ones. If the `selected` attribute assertion in `TestAuxesTabRendersLayoutSelect` fails on exact whitespace, loosen it to `strings.Contains(body, id)` plus a separate `strings.Contains(body, "selected")` check within the substring around it — the point is proving the id round-trips into the rendered option, not pinning exact `html/template` whitespace output.

- [ ] **Step 7: Commit**

```bash
git add web/admin/sources.html.tmpl web/admin/auxes.html.tmpl web/admin/admin.js web/admin/admin.css internal/admin/admin_test.go
git commit -m "$(cat <<'EOF'
Add Sources tab layout picker/management UI, Auxes tab per-aux layout select

Sources tab gets a layout <select> (switches via GET /admin/sources),
a rename field and Delete button for non-default layouts, and a "+ New
layout" form. Auxes tab gets a per-aux layout <select> riding the
existing auto-submit form. Both are declarative htmx, no new JS beyond
carrying layoutId through the hover "+" section button's ajax call.
EOF
)"
```

---

## Task 7: Mixer client — per-aux layout switching

**Files:**
- Modify: `web/mixer/mixer.js`

**Interfaces:**
- Consumes: `env.config.mixerLayouts` (wire, Task 2), `env.config.auxes[].layoutId` (wire, Task 2).

- [ ] **Step 1: Add a module-level `mixerLayouts` var and a `layoutFor` helper**

In `web/mixer/mixer.js`, change the `let` block near the top:

```js
let ws = null,
reconnectTimer = null,
sources = [],
auxes = [],
mixerLayouts = {},
selectedAux = null; // the full aux object (channel/label/colour/icon/stereo/layoutId), not just an id
```

Add near `sourceKey`/`addressRoot` (small pure helpers):

```js
// layoutFor resolves which named layout an aux renders — an aux with no
// layoutId (or one naming a layout that's since been deleted admin-side)
// falls back to "default", mirroring config.Aux.LayoutID's doc comment.
function layoutFor(aux) {
	return (aux && mixerLayouts[aux.layoutId]) || mixerLayouts["default"] || [];
}
```

- [ ] **Step 2: Call `buildSources` from `selectAux`/`buildAux` instead of once in `onMessage`**

Replace `selectAux`:

```js
function selectAux(channel) {
	const aux = auxes.find((a) => String(a.channel) === String(channel));
	if (!aux) return;
	selectedAux = aux;
	localStorage.setItem("aux", String(channel));
	document.body.classList.remove("picking-aux");
	document.body.style.setProperty("--tint", hexToRgb(aux.colour) || defaultTint);
	auxIcon.src = iconSrc(aux.icon);
	auxLabel.textContent = aux.label || ("Aux " + aux.channel);
	panToggleWrap.hidden = !aux.stereo;
	panToggle.checked = false;
	document.body.classList.remove("panning");
	buildSources(layoutFor(aux));
	resetSliders();
	requestValues();
}
```

Replace `buildAux`'s `else` branch (the "no valid saved aux" case):

```js
function buildAux(list) {
	auxes = list || [];
	let html = "";
	for (const a of auxes) {
		if (!a.enabled) continue;
		html += '<button type="button" class="aux-button" data-channel="' + a.channel + '" style="--tint:' + (hexToRgb(a.colour) || defaultTint) + '">';
		if (a.icon) html += '<img src="' + iconSrc(a.icon) + '" alt="" />';
		html += '<span>' + escapeHtml(a.label || ("Aux " + a.channel)) + '</span></button>';
	}
	auxPickerDiv.innerHTML = html;

	const saved = localStorage.getItem("aux");
	const stillValid = saved && auxes.some((a) => a.enabled && String(a.channel) === saved);
	if (stillValid) {
		selectAux(saved);
	} else {
		selectedAux = null;
		auxIcon.src = "";
		auxLabel.textContent = "Select aux…";
		panToggleWrap.hidden = true;
		buildSources(layoutFor(null));
		document.body.classList.add("picking-aux");
	}
}
```

Replace `onMessage`'s `"config"` branch — remove the direct `buildSources` call (now handled inside `buildAux`/`selectAux`) and stash `mixerLayouts`:

```js
	if (env.kind === "config") {
		snapshotSpan.textContent = (env.config && env.config.snapshot) || "";
		mixerLayouts = (env.config && env.config.mixerLayouts) || {};
		buildAux(env.config && env.config.auxes);
		requestValues();
		return;
	}
```

`buildSources`'s own signature/body is unchanged — it still takes a layout array and renders it; it's just now called with `layoutFor(aux)` instead of the old single `env.config.mixerLayout`.

- [ ] **Step 3: Manual browser verification**

There's no JS test runner in this repo (confirmed: no `package.json`/`*.test.js`) — server-rendered Go tests cover the admin HTML, but the mixer client's runtime behavior needs a real browser check. This step is folded into Task 8's end-to-end verification rather than repeated here, since it needs a running server with at least two named layouts and two auxes to be meaningful.

- [ ] **Step 4: Commit**

```bash
git add web/mixer/mixer.js
git commit -m "$(cat <<'EOF'
Switch the mixer client's source layout per selected aux

The client now keeps every named layout from env.config.mixerLayouts and
resolves which one to render (layoutFor) each time an aux is selected —
entirely client-side, no new server round trip, since aux selection
already happened client-side before this change.
EOF
)"
```

---

## Task 8: Version bump + full verification + manual smoke test

**Files:**
- Modify: `VERSION`

**Interfaces:**
- Consumes: everything from Tasks 1-7.

- [ ] **Step 1: Bump `VERSION`**

Read the current value (should be `0.3.6` per the version this plan was written against) and bump the minor version for this feature (new user-facing capability, backward-compatible on-disk format via the shim): `0.4.0`.

```bash
echo "0.4.0" > VERSION
```

- [ ] **Step 2: Full build and test suite**

```bash
go build ./...
go vet ./...
go test ./...
```

Expected: all pass, no vet warnings.

- [ ] **Step 3: Manual smoke test in a browser**

Use the `run` skill (or start the server directly per its usual local-dev invocation) and, against a running instance:

1. Open `/admin`, Sources tab. Confirm the layout picker shows "Default" and no rename/delete controls.
2. Click "+ New layout", name it "Monitor Mix". Confirm the tab switches to it and it starts with the same cards Default had.
3. Reorder/hide a couple of sources in "Monitor Mix" so it visibly differs from Default (e.g. hide channel 1, reorder channel 2 above channel 3).
4. Switch the picker back to "Default" — confirm Default's own cards/order are unaffected by the Monitor Mix edits.
5. Go to the Auxes tab. Set one aux's layout select to "Monitor Mix", leave another on "Default".
6. Open the mixer client (`/`) in two browser tabs/windows, pick the Monitor Mix aux in one and the Default aux in the other. Confirm the source list differs between the two tabs exactly as edited in step 3, and that faders/sends still work in both.
7. Back in `/admin`, rename "Monitor Mix" to something else, confirm the Auxes tab's select label updates on next load and the mixer client (on its next config push, e.g. reload) shows the same layout content under the new name.
8. Delete the renamed layout. Confirm no error, the Sources tab lands back on Default, and the aux that was pointed at it now renders Default's layout in the mixer client (reload that tab to confirm).
9. Save a preset, reset, load the preset back — confirm both layouts and the aux-to-layout assignment are restored.

Report any deviation from the above before considering this task done — this is the one part of the feature no Go test exercises end to end (the client-side `layoutFor` switch and the full admin round trip through real HTML/JS in a browser).

- [ ] **Step 4: Commit**

```bash
git add VERSION
git commit -m "$(cat <<'EOF'
Bump version to 0.4.0 for aux-specific mixer layouts
EOF
)"
```
