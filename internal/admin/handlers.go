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

func formLayoutID(r *http.Request) string {
	if id := r.FormValue("layoutId"); id != "" {
		return id
	}
	return oscmsg.DefaultLayoutID
}

func queryLayoutID(r *http.Request) string {
	if id := r.URL.Query().Get("layout"); id != "" {
		return id
	}
	return oscmsg.DefaultLayoutID
}

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
	state, err := h.cfg.LoadStateOrDefault()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data := ViewData{
		Global:      global,
		Network:     NetworkSectionData{Global: global, DeskStatus: status},
		SourcesData: sourcesSectionData(cfg, queryLayoutID(r), false),
		AuxesData:   auxesSectionData(cfg, h.icons, false),
		Presets:     toPresetRows(presets, state.UUID),
		Message:     r.URL.Query().Get("msg"),
		Version:     h.version,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}

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

func (h *Handler) handleSourcesView(w http.ResponseWriter, r *http.Request) {
	layoutID := queryLayoutID(r)

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
	source, ok := state.MixerLayouts[fromID]
	if !ok {

		source = state.MixerLayouts[config.DefaultLayoutID]
	}
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
	if err := h.renderAuxesSection(w, cfg, true); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}

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
	if err := h.renderAuxesSection(w, cfg, true); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}

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
	if err := h.renderAuxesSection(w, cfg, true); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}

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
}

func layoutItemKey(it config.LayoutItem) string {
	if it.Kind == config.LayoutKindSection {
		return "section:" + it.SectionID
	}
	return "source:" + it.Type + ":" + strconv.Itoa(it.Number)
}

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

func (h *Handler) handleLockUpdate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	locked := r.Form.Has("locked")

	select {
	case h.adminEvents <- hub.AdminEvent{Kind: hub.AdminSetLocked, Locked: locked}:
	default:
		h.log.Printf("admin lock: event channel full, not applied")
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "lock-toggle", hub.DeskStatusInfo{Locked: locked}); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}

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
		MixerLayouts: configLayoutsFromPayload(cfg.MixerLayouts),
		Auxes:        configAuxesFromPayload(cfg.Auxes),
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
	cfg, err := h.queryConfig(ctx)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderPresetsSection(w); err != nil {
		h.log.Printf("admin render: %v", err)
	}
	if err == nil {
		if err := h.renderSourcesSection(w, cfg, oscmsg.DefaultLayoutID, true); err != nil {
			h.log.Printf("admin render: %v", err)
		}
		if err := h.renderAuxesSection(w, cfg, true); err != nil {
			h.log.Printf("admin render: %v", err)
		}
	}
}

func (h *Handler) handlePresetUpdate(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("uuid")

	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	cfg, err := h.queryConfig(ctx)
	if err != nil {
		http.Error(w, "hub not responding: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	_, err = h.cfg.UpdatePreset(uuid, configLayoutsFromPayload(cfg.MixerLayouts), configAuxesFromPayload(cfg.Auxes))
	if errors.Is(err, config.ErrPresetNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderPresetsSection(w); err != nil {
		h.log.Printf("admin render: %v", err)
	}
}

func (h *Handler) handlePresetsReset(w http.ResponseWriter, r *http.Request) {
	def := config.NewDefaultState()
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
		if err := h.renderSourcesSection(w, cfg, oscmsg.DefaultLayoutID, true); err != nil {
			h.log.Printf("admin render: %v", err)
		}
		if err := h.renderAuxesSection(w, cfg, true); err != nil {
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
