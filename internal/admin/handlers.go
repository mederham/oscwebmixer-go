package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/mederham/oscwebmixer-go/internal/config"
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
	global, err := h.cfg.LoadGlobalOrDefault()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	presets, err := h.cfg.LoadPresets()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data := ViewData{
		Global:      global,
		SourcesData: SourcesSectionData{Sources: toSourceRows(cfg.Sources)},
		AuxesData:   AuxesSectionData{Auxes: toAuxRows(cfg.Auxes)},
		Presets:     toPresetRows(presets),
		Message:     r.URL.Query().Get("msg"),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}

// handleSourceUpdate saves one source's enabled/title/icon. Order is
// untouched here — see handleSourceMove/handleSourcesReorder.
func (h *Handler) handleSourceUpdate(w http.ResponseWriter, r *http.Request) {
	sourceType := r.PathValue("type")
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
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

	sources := configSourcesFromPayload(cfg.Sources)
	found := false
	for i := range sources {
		if sources[i].Type == sourceType && sources[i].Number == number {
			sources[i].Enabled = r.Form.Has("enabled")
			sources[i].Title = r.FormValue("title")
			sources[i].Icon = r.FormValue("icon")
			found = true
			break
		}
	}
	if !found {
		http.NotFound(w, r)
		return
	}

	state, err := h.cfg.LoadStateOrDefault()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	state.Sources = sources
	state.Auxes = configAuxesFromPayload(cfg.Auxes)
	if err := h.cfg.SaveState(state); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.applyState(state)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderSourcesSection(w, sourcesToPayload(sources, cfg.Sources), false); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}

// handleSourcesReorder is driven by the drag-and-drop UI (see
// web/admin/admin.js): it receives the full new ordering as a
// comma-separated "type:number" list and rewrites every source's
// DisplayOrder to match position in that list.
func (h *Handler) handleSourcesReorder(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	orderParam := r.FormValue("order")
	var keys []string
	if orderParam != "" {
		keys = strings.Split(orderParam, ",")
	}

	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	cfg, err := h.queryConfig(ctx)
	if err != nil {
		http.Error(w, "hub not responding: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	sources := configSourcesFromPayload(cfg.Sources)
	byKey := make(map[string]*config.Source, len(sources))
	for i := range sources {
		byKey[sourceKeyStr(sources[i])] = &sources[i]
	}
	for i, k := range keys {
		if s, ok := byKey[k]; ok {
			s.DisplayOrder = i
		}
	}
	sort.SliceStable(sources, func(a, b int) bool { return sources[a].DisplayOrder < sources[b].DisplayOrder })

	state, err := h.cfg.LoadStateOrDefault()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	state.Sources = sources
	state.Auxes = configAuxesFromPayload(cfg.Auxes)
	if err := h.cfg.SaveState(state); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.applyState(state)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderSourcesSection(w, sourcesToPayload(sources, cfg.Sources), false); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}

func sourceKeyStr(s config.Source) string {
	return fmt.Sprintf("%s:%d", s.Type, s.Number)
}

// sourcesToPayload re-attaches the live/desk-derived Label and Stereo back
// onto the just-edited config.Source list, using the last-known values from
// cfg — avoids a second hub round trip purely to re-render.
func sourcesToPayload(sources []config.Source, labeled []oscmsg.SourceConfig) []oscmsg.SourceConfig {
	byKey := make(map[string]oscmsg.SourceConfig, len(labeled))
	for _, s := range labeled {
		byKey[fmt.Sprintf("%s:%d", s.Type, s.Number)] = s
	}
	out := make([]oscmsg.SourceConfig, len(sources))
	for i, s := range sources {
		live := byKey[fmt.Sprintf("%s:%d", s.Type, s.Number)]
		out[i] = oscmsg.SourceConfig{
			Type: s.Type, Number: s.Number, Label: live.Label, Stereo: live.Stereo,
			Enabled: s.Enabled, Order: s.DisplayOrder, Title: s.Title, Icon: s.Icon,
		}
	}
	return out
}

func (h *Handler) handleAuxUpdate(w http.ResponseWriter, r *http.Request) {
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
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
	idx := number - 1
	if idx < 0 || idx >= len(auxes) {
		http.NotFound(w, r)
		return
	}
	auxes[idx] = config.Aux{
		Enabled: r.Form.Has("enabled"),
		Colour:  r.FormValue("colour"),
		Icon:    r.FormValue("icon"),
	}

	state, err := h.cfg.LoadStateOrDefault()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	state.Sources = configSourcesFromPayload(cfg.Sources)
	state.Auxes = auxes
	if err := h.cfg.SaveState(state); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.applyState(state)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	auxPayload := make([]oscmsg.AuxConfig, len(auxes))
	for i, a := range auxes {
		label := ""
		if i < len(cfg.Auxes) {
			label = cfg.Auxes[i].Label
		}
		auxPayload[i] = oscmsg.AuxConfig{
			Channel: i + 1, Label: label, Enabled: a.Enabled,
			Stereo: i < len(cfg.Auxes) && cfg.Auxes[i].Stereo, Colour: a.Colour, Icon: a.Icon,
		}
	}
	if err := h.renderAuxesSection(w, auxPayload, false); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}

// handleNetworkUpdate saves desk/server/OSC settings — deliberately
// save-only, no live reconfiguration (see package doc's scope note).
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

	global.Debug = r.Form.Has("debug")
	global.Desk.IP = strings.TrimSpace(r.FormValue("deskIP"))
	global.Desk.Port = formInt(r.FormValue("deskPort"), global.Desk.Port)
	global.OSC.Port = formInt(r.FormValue("oscPort"), global.OSC.Port)
	global.Server.Port = formInt(r.FormValue("serverPort"), global.Server.Port)

	if err := h.cfg.SaveGlobal(global); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "network-section", global); err != nil {
		h.log.Printf("admin render: %v", err)
	}
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

	global, err := h.cfg.LoadGlobalOrDefault()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	global.ExternalDevices = append(global.ExternalDevices, config.ExternalDevice{
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

func (h *Handler) handleExternalDeviceDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	global, err := h.cfg.LoadGlobalOrDefault()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	filtered := global.ExternalDevices[:0]
	for _, d := range global.ExternalDevices {
		if d.Name != name {
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
		Sources: configSourcesFromPayload(cfg.Sources),
		Auxes:   configAuxesFromPayload(cfg.Auxes),
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

// handlePresetLoad changes sources AND auxes at once, but only the
// presets section is the actual hx-target — the other two are pushed as
// out-of-band swaps in the same response (see SourcesSectionData/
// AuxesSectionData's OOB field).
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
		if err := h.renderSourcesSection(w, cfg.Sources, true); err != nil {
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
