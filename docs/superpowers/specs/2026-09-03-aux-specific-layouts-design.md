# Aux-specific mixer layouts — design

## Problem

The mixer's source layout (order, section markers, enabled/icon per
channel/group) is one global list, editable from the Sources tab and shared
by every aux a client can select. There's no way to give one aux (e.g. a
monitor mix) a different source order/visibility than another.

## Goal

Let the admin define multiple named layouts (Sources tab), assign one to
each aux (Auxes tab, default: the "Default" layout), and have the mixer
client render the right layout when a user switches aux — persisted in both
`state.json` and `presets.json`.

## Key existing-architecture fact this design leans on

`ConfigPayload` (the WS wire type) is pushed identically to every connected
client, both on connect (`client.go`) and on any admin apply (`hub/admin.go`).
There is no per-client server-side customization today, and aux selection
already happens entirely client-side (`mixer.js`'s `selectAux`/`localStorage`).
So: send **every** named layout to every client in one payload, and have the
client pick which one to render locally when the aux changes. This avoids
inventing any per-client server-side targeting.

## A. Data model & wire protocol

`internal/config/types.go`:

```go
type NamedLayout struct {
	Name  string       `json:"name"`
	Items []LayoutItem `json:"items"`
}

type State struct {
	UUID        string                 `json:"uuid"`
	Name        string                 `json:"name"`
	SessionName string                 `json:"sessionName"`
	Auxes       []Aux                  `json:"auxes"`
	MixerLayouts map[string]NamedLayout `json:"mixerLayouts"`
}

type Aux struct {
	Enabled  bool   `json:"enabled"`
	Colour   string `json:"colour"`
	Icon     string `json:"icon"`
	LayoutID string `json:"layoutId"` // "" == "default"
}
```

- Key is a generated id (`config.NewID()`, same convention as sections/
  presets/external devices) — **except** the always-present entry, whose key
  is the literal string `"default"` (not a random id), `Name: "Default"`.
  Code enforces `"default"` can't be renamed or deleted.
- `Preset` needs no struct change — it embeds `State`.

`internal/oscmsg/config.go`:

```go
type ConfigPayload struct {
	MixerLayouts map[string][]LayoutItem `json:"mixerLayouts"`
	Auxes        []AuxConfig             `json:"auxes"`
	Snapshot     string                  `json:"snapshot"`
}

type AuxConfig struct {
	Channel  int    `json:"channel"`
	Label    string `json:"label"`
	Enabled  bool   `json:"enabled"`
	Stereo   bool   `json:"stereo"`
	Colour   string `json:"colour"`
	Icon     string `json:"icon"`
	LayoutID string `json:"layoutId"`
}
```

The wire shape skips `Name` — the mixer client only needs to pick a map
entry by id, never displays a layout's name.

## B. Hub / buildConfig

- `Hub.layout []config.LayoutItem` → `Hub.layouts map[string]config.NamedLayout`.
- `AdminEvent.Layout` → `AdminEvent.Layouts map[string]config.NamedLayout`.
- `Options.InitialLayout` → `Options.InitialLayouts`.
- `buildConfig()`'s per-source merge (live label/stereo overlay, plus the
  natural-desk-order fallback appending any source not mentioned in the
  layout) currently runs once. It now runs once per entry in `h.layouts`,
  producing `ConfigPayload.MixerLayouts[id]`. Same merge logic, looped — each
  named layout is independently sparse and gets the same fallback treatment
  `"default"` gets today.
- `AuxConfig.LayoutID` copied straight from `h.auxOverrides[i].LayoutID`.

## C. Admin backend + Sources/Auxes tab UX

**Sources tab** gets a layout picker above the list:
- A `<select>` listing every layout by name ("Default" first, then others
  in whatever order the map iterates — sorted by name for stable display).
- Switching it does an htmx `GET /admin/sources?layout={id}`, re-rendering
  `#section-sources` scoped to that layout (cards, dropdown reflecting the
  new selection, and a hidden `layoutId` field inside `#sources-form` so
  every subsequent save knows which layout it's editing).
- "+ New layout": `POST /admin/layouts` (name required, clones the
  currently-selected layout's items as a starting point), then switches the
  picker to the new layout.
- When a non-default layout is selected: "Rename" (`POST
  /admin/layouts/{id}/rename`) and "Delete" (`POST
  /admin/layouts/{id}/delete`) actions appear. Deleting the
  currently-viewed layout switches the picker back to "Default". Deleting a
  layout does **not** check aux usage — any aux whose `LayoutID` no longer
  resolves in `mixerLayouts` renders with `"default"`, same as an aux that
  never had a custom layout (mirrors buildConfig's existing "no entry ==
  default behavior" pattern elsewhere).
- `handleSourcesSave`/`handleSourcesReset`/`handleSectionAdd`/
  `handleSectionDelete` all read `layoutId` (form field or query param) and
  mutate `state.MixerLayouts[layoutId].Items` specifically instead of the
  old single list.

**Auxes tab**: each aux card gets `<select name="layout[N]">` listing the
same layouts, defaulting to `"default"`. Rides the existing auto-submit
`#auxes-form` (no new endpoint) — `handleAuxesSave` reads
`layout[N]` alongside `enabled[N]`/`colour[N]`/`icon[N]`.

## D. Mixer client (`web/mixer/mixer.js`)

- On receiving config, the client keeps the whole `env.config.mixerLayouts`
  map (not just one array) plus `env.config.auxes` (each aux now carries
  `layoutId`).
- `selectAux()` calls `buildSources(mixerLayouts[aux.layoutId] ||
  mixerLayouts["default"])` so switching aux re-renders the source list
  entirely client-side — no extra server round trip.
- Initial load (before any aux is picked, or restoring a saved aux from
  `localStorage`) calls `buildSources()` once with the resolved layout for
  whatever aux ends up selected (or `"default"` while the aux picker is
  showing).

## E. Legacy `state.json`/`presets.json` compat shim

Old files have `"mixerLayout": [...]`; once the field is renamed, Go's
unmarshal silently ignores that key and `MixerLayouts` comes back nil —
silently dropping existing layouts/presets on upgrade. Shim in
`LoadStateOrDefault` (and per-preset in `LoadPresets`):

```go
// ponytail: one-time compat shim for the mixerLayout -> mixerLayouts
// rename (2026-09). Safe to delete once you're confident every
// state.json/presets.json on disk has been resaved in the new shape
// (happens automatically on the first admin edit after upgrading).
if len(s.MixerLayouts) == 0 {
	var legacy struct {
		MixerLayout []LayoutItem `json:"mixerLayout"`
	}
	if json.Unmarshal(data, &legacy) == nil && len(legacy.MixerLayout) > 0 {
		s.MixerLayouts = map[string]NamedLayout{
			"default": {Name: "Default", Items: legacy.MixerLayout},
		}
	}
}
```

Re-parses the same raw bytes into a throwaway struct that only looks for the
old key, only when the new field came back empty. No version field, no
migration registry. Runs on every load but is a no-op once a file has
actually been resaved in the new format — permanent-but-inert rather than
self-cleaning, which is fine at this project's scale (single-operator local
tool, not a fleet of installs).

## Config version migration (discussed, not implemented — per request)

Not building this now. This project's schema changes are almost always
additive (new field, sane zero-value default — JSON handles that with zero
code) with rare renames like this one. A `configVersion` field + a
registered chain of migration functions is the right answer once
breaking renames start happening often enough that ad hoc shims get hard to
order/reason about. At this project's current scale (single-operator local
tool, one breaking rename in its history), a versioned migration framework
is speculative infrastructure. Revisit if a second or third breaking
change lands in close succession, or if shims start conflicting with each
other.

## Files touched

- `internal/config/types.go`, `config.go` (shim), `config_test.go`
- `internal/oscmsg/config.go`
- `internal/hub/hub.go`, `admin.go`, `buildconfig.go`, `hub_test.go`
- `internal/admin/handlers.go`, `view.go`, `admin.go` (routes)
- `internal/admin/admin_test.go`
- `web/admin/sources.html.tmpl`, `auxes.html.tmpl`, `admin.js`
- `web/mixer/mixer.js`
- `cmd/oscwebmixer/main.go` (Options field rename)
- `VERSION` bump (per standing project convention)
