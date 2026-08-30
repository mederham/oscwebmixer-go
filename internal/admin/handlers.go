package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/mederham/oscwebmixer-go/internal/config"
	"github.com/mederham/oscwebmixer-go/internal/hub"
	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
)

func (h *Handler) handleIndex(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()

	cfg, err := h.queryConfig(ctx)
	if err != nil {
		http.Error(w, "hub not responding: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	status, err := h.queryStatus(ctx)
	if err != nil {
		http.Error(w, "hub not responding: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	global, err := h.cfg.LoadGlobalOrDefault()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if backfillExternalDeviceIDs(&global) {
		if err := h.cfg.SaveGlobal(global); err != nil {
			h.log.Printf("admin: backfill external device IDs: %v", err)
		}
	}
	presets, err := h.cfg.LoadPresets()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data := ViewData{
		Global:      global,
		Network:     NetworkSectionData{Global: global, DeskStatus: status},
		SourcesData: toSourcesSectionData(cfg.MixerLayout, false),
		AuxesData:   AuxesSectionData{Auxes: toAuxRows(cfg.Auxes), Icons: h.icons},
		Presets:     toPresetRows(presets),
		Message:     r.URL.Query().Get("msg"),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}

// handleSourcesSave is the single endpoint for everything the Sources
// tab's one <form> can produce: reordering (Sortable's onEnd), the enabled
// toggle, and renaming a section (its "change" event, which only fires
// once the value actually differs — see admin.js) all auto-submit and
// apply immediately, no separate Save button — every edit here is just
// "the Sources form was submitted," this one endpoint either way. The
// posted "layout" field is a JSON
// array (built by admin.js's collectLayout, injected via htmx:configRequest
// — see this package's doc comment for why a plain named-field form can't
// represent an ordered, mixed-kind list) mirroring oscmsg.LayoutItem.
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

	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	cfg, err := h.queryConfig(ctx)
	if err != nil {
		http.Error(w, "hub not responding: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	fresh, err := h.saveState(ctx, layoutFromPayload(wireLayout), configAuxesFromPayload(cfg.Auxes))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderSourcesSection(w, fresh.MixerLayout, false); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}

// handleSourcesReset clears the mixer layout entirely. buildConfig's own
// fallback for anything not mentioned in the layout — walk channels 1..N
// ascending, then groups 1..M ascending, in natural desk order — already
// produces exactly "all channels then groups, ascending" once there's
// nothing explicit to override it with, so resetting is just persisting
// an empty layout; there's no default sequence to construct here.
//
// Deliberately a full clear, not a "drop only stale/out-of-range entries"
// repair: after a real session change, the old layout usually doesn't
// apply anymore anyway (different channel names/counts/purpose), so
// starting over is faster than hunting down what's still relevant.
func (h *Handler) handleSourcesReset(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	cfg, err := h.queryConfig(ctx)
	if err != nil {
		http.Error(w, "hub not responding: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	fresh, err := h.saveState(ctx, nil, configAuxesFromPayload(cfg.Auxes))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderSourcesSection(w, fresh.MixerLayout, false); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}

// handleSectionAdd creates a new section marker — at the end of the
// layout by default (the bottom "+ Add section" form), or immediately
// before a specific row when "beforeKey" is posted (the hover "+" button
// on a source-card — see admin.js; section-markers don't render one, but
// layoutItemKey/matching below stays general rather than source-only, so
// a "section:id" key still works if ever posted some other way), so a
// section can land exactly where it's wanted without a drag across a
// layout that's often taller than the viewport. beforeKey matches
// layoutItemKey's format exactly; an empty or unmatched key falls back to
// appending at the end.
func (h *Handler) handleSectionAdd(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `<p class="flash error">Section name is required.</p>`)
		return
	}

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

	layout := layoutFromPayload(cfg.MixerLayout)
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

	fresh, err := h.saveState(ctx, inserted, configAuxesFromPayload(cfg.Auxes))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderSourcesSection(w, fresh.MixerLayout, false); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}

// layoutItemKey identifies one layout item the same way admin.js's
// data-before-key attributes do (see sources.html.tmpl) — "source:type:
// number" or "section:id" — so handleSectionAdd can find exactly which
// row a hover "+" button was clicked on.
func layoutItemKey(it config.LayoutItem) string {
	if it.Kind == config.LayoutKindSection {
		return "section:" + it.SectionID
	}
	return "source:" + it.Type + ":" + strconv.Itoa(it.Number)
}

// handleSectionDelete removes one section marker; nothing else needs
// adjusting — the sources that were "in" it were only ever derived from
// their position relative to this marker, not linked to it by a stored
// field, so removing the marker alone is the whole operation.
func (h *Handler) handleSectionDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	cfg, err := h.queryConfig(ctx)
	if err != nil {
		http.Error(w, "hub not responding: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	layout := layoutFromPayload(cfg.MixerLayout)
	filtered := layout[:0]
	for _, it := range layout {
		if it.Kind == config.LayoutKindSection && it.SectionID == id {
			continue
		}
		filtered = append(filtered, it)
	}

	fresh, err := h.saveState(ctx, filtered, configAuxesFromPayload(cfg.Auxes))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderSourcesSection(w, fresh.MixerLayout, false); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}

// handleAuxesSave replaces per-row saving with one batch save for the
// whole Auxes table — array position is already the channel identity
// (configAuxesFromPayload's existing Auxes[i].Channel == i+1 convention),
// so no hidden tracking fields are needed here at all, unlike Sources.
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
	}

	fresh, err := h.saveState(ctx, layoutFromPayload(cfg.MixerLayout), auxes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderAuxesSection(w, fresh.Auxes, false); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}

// handleNetworkUpdate saves Desk IP/Desk port/OSC listen port and applies
// all three live via AdminNetworkApply — no restart needed (see
// hub.applyNetworkChange, which only actually touches the desk connection
// for whichever field really changed). The HTTP server port isn't on this
// form at all: rebinding the very listener serving this request, from
// inside a handler of that same listener, isn't something to support —
// set it via the OSCWEBMIXER_HTTP_PORT environment variable instead (see
// main.go), which does need a restart, same as before. Debug logging
// lives on the Settings tab (see handleSettingsUpdate), not here.
func (h *Handler) handleNetworkUpdate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	global, err := h.cfg.LoadGlobalOrDefault()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	global.Desk.IP = strings.TrimSpace(r.FormValue("deskIP"))
	global.Desk.Port = formInt(r.FormValue("deskPort"), global.Desk.Port)
	global.OSC.Port = formInt(r.FormValue("oscPort"), global.OSC.Port)

	if err := h.cfg.SaveGlobal(global); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	select {
	case h.adminEvents <- hub.AdminEvent{
		Kind:   hub.AdminNetworkApply,
		DeskIP: global.Desk.IP, DeskPort: global.Desk.Port, OSCPort: global.OSC.Port,
	}:
	default:
		// The saved file is already correct regardless — see applyState's
		// identical fallback for why this only means "not live yet."
		h.log.Printf("admin apply: event channel full, network settings saved to disk but not yet applied live")
	}

	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	status, err := h.queryStatus(ctx)
	if err != nil {
		http.Error(w, "hub not responding: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "network-section", NetworkSectionData{Global: global, DeskStatus: status}); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}

// handleNetworkStatus renders just the status-dot fragment — polled by
// network.html.tmpl every 5s (hx-trigger="load, every 5s") so the
// indicator stays live without disturbing the rest of the form.
func (h *Handler) handleNetworkStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	status, err := h.queryStatus(ctx)
	if err != nil {
		http.Error(w, "hub not responding: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "status-dot", status); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}

// handleSettingsUpdate saves the auto-hide name patterns (see
// config.Global's AutoHide*Patterns fields) and Debug logging, and applies
// both live via the same AdminApply path as external devices — no socket
// involved here, unlike the Network tab's Desk IP/port/OSC listen port
// (see handleNetworkUpdate/AdminNetworkApply), so this just rides the
// existing state-apply flow rather than needing its own event kind.
func (h *Handler) handleSettingsUpdate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	global, err := h.cfg.LoadGlobalOrDefault()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	global.AutoHideChannelPatterns = parsePatterns(r.FormValue("autoHideChannelPatterns"))
	global.AutoHideGroupPatterns = parsePatterns(r.FormValue("autoHideGroupPatterns"))
	global.AutoHideAuxPatterns = parsePatterns(r.FormValue("autoHideAuxPatterns"))
	global.Debug = r.Form.Has("debug")
	if err := h.cfg.SaveGlobal(global); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if state, err := h.cfg.LoadStateOrDefault(); err == nil {
		h.applyState(state)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "settings-section", global); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}

// parsePatterns splits a comma-separated form field into trimmed,
// non-empty patterns.
func parsePatterns(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (h *Handler) handleExternalDeviceAdd(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `<p class="flash error">Device name is required.</p>`)
		return
	}

	id, err := config.NewID()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	global, err := h.cfg.LoadGlobalOrDefault()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	global.ExternalDevices = append(global.ExternalDevices, config.ExternalDevice{
		ID:        id,
		Name:      name,
		IP:        strings.TrimSpace(r.FormValue("ip")),
		Port:      formInt(r.FormValue("port"), 0),
		Broadcast: r.Form.Has("broadcast"),
		Loopback:  r.Form.Has("loopback"),
	})
	if err := h.cfg.SaveGlobal(global); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if state, err := h.cfg.LoadStateOrDefault(); err == nil {
		h.applyState(state)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "external-devices-section", global); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}

// handleExternalDeviceUpdate saves one row's edited fields in place —
// keyed by ID (not Name) so renaming a device doesn't invalidate its own
// edit URL mid-edit.
func (h *Handler) handleExternalDeviceUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	global, err := h.cfg.LoadGlobalOrDefault()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	found := false
	for i := range global.ExternalDevices {
		if global.ExternalDevices[i].ID == id {
			global.ExternalDevices[i].Name = strings.TrimSpace(r.FormValue("name"))
			global.ExternalDevices[i].IP = strings.TrimSpace(r.FormValue("ip"))
			global.ExternalDevices[i].Port = formInt(r.FormValue("port"), global.ExternalDevices[i].Port)
			global.ExternalDevices[i].Broadcast = r.Form.Has("broadcast")
			global.ExternalDevices[i].Loopback = r.Form.Has("loopback")
			found = true
			break
		}
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	if err := h.cfg.SaveGlobal(global); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if state, err := h.cfg.LoadStateOrDefault(); err == nil {
		h.applyState(state)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "external-devices-section", global); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}

func (h *Handler) handleExternalDeviceDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	global, err := h.cfg.LoadGlobalOrDefault()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	filtered := global.ExternalDevices[:0]
	for _, d := range global.ExternalDevices {
		if d.ID != id {
			filtered = append(filtered, d)
		}
	}
	global.ExternalDevices = filtered
	if err := h.cfg.SaveGlobal(global); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if state, err := h.cfg.LoadStateOrDefault(); err == nil {
		h.applyState(state)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "external-devices-section", global); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}

func (h *Handler) handlePresetSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `<p class="flash error">Preset name is required.</p>`)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	cfg, err := h.queryConfig(ctx)
	if err != nil {
		http.Error(w, "hub not responding: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	state := config.State{
		MixerLayout: layoutFromPayload(cfg.MixerLayout),
		Auxes:       configAuxesFromPayload(cfg.Auxes),
	}
	if _, err := h.cfg.SaveCurrentStateAsPreset(state, name); err != nil {
		if errors.Is(err, config.ErrPresetExists) {
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `<p class="flash error">A preset with that name already exists.</p>`)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderPresetsSection(w); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}

// handlePresetLoad changes both the mixer layout and auxes at once, but
// only the presets section is the actual hx-target — the other rides
// along as an htmx out-of-band swap in the same response (see
// SourcesSectionData/AuxesSectionData's OOB field).
func (h *Handler) handlePresetLoad(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("uuid")
	preset, err := h.cfg.GetPresetByID(uuid)
	if errors.Is(err, config.ErrPresetNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := h.cfg.SaveState(preset.State); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.applyState(preset.State)

	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	cfg, err := h.queryConfig(ctx) // fresh labels post-apply

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderPresetsSection(w); err != nil {
		h.log.Printf("admin render: %v", err)
	}
	if err == nil {
		if err := h.renderSourcesSection(w, cfg.MixerLayout, true); err != nil {
			h.log.Printf("admin render: %v", err)
		}
		if err := h.renderAuxesSection(w, cfg.Auxes, true); err != nil {
			h.log.Printf("admin render: %v", err)
		}
	}
}

// handlePresetsReset clears the current mixer layout and aux overrides
// back to defaults and applies it live — the same "start over" action as
// loading a preset, just loading the zero-value default instead of a
// saved one (mirrors handlePresetLoad's OOB-update shape since it changes
// sources and auxes too, not just the presets list). Deliberately scoped
// to state.json only: saved presets, external devices, and network
// settings are untouched, and the running process is not restarted.
func (h *Handler) handlePresetsReset(w http.ResponseWriter, r *http.Request) {
	def := config.State{Name: "Default"}
	if err := h.cfg.SaveState(def); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.applyState(def)

	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	cfg, err := h.queryConfig(ctx)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderPresetsSection(w); err != nil {
		h.log.Printf("admin render: %v", err)
	}
	if err == nil {
		if err := h.renderSourcesSection(w, cfg.MixerLayout, true); err != nil {
			h.log.Printf("admin render: %v", err)
		}
		if err := h.renderAuxesSection(w, cfg.Auxes, true); err != nil {
			h.log.Printf("admin render: %v", err)
		}
	}
}

func (h *Handler) handlePresetDelete(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("uuid")
	if err := h.cfg.DeletePreset(uuid); err != nil && !errors.Is(err, config.ErrPresetNotFound) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderPresetsSection(w); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}

func formInt(s string, fallback int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return n
}
