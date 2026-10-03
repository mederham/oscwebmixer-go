package admin

import (
	"context"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/mederham/oscwebmixer-go/internal/config"
	"github.com/mederham/oscwebmixer-go/internal/hub"
	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
)

const queryTimeout = 3 * time.Second

type Handler struct {
	cfg         *config.Manager
	adminEvents chan<- hub.AdminEvent
	log         *log.Logger
	tmpl        *template.Template

	icons []string

	version string
}

var templateFuncs = template.FuncMap{
	"dotClass":     dotClass,
	"dotLabel":     dotLabel,
	"sinceRounded": func(t time.Time) string { return time.Since(t).Round(time.Second).String() },
	"iconLabel":    iconLabel,
	"badge":        badge,
	"join":         strings.Join,
	"prettyDate":   prettyDate,
}

func prettyDate(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Local().Format("Jan 2, 2006 3:04 PM")
}

func iconLabel(filename string) string {
	name := strings.TrimSuffix(filename, ".svg")
	words := strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' })
	for i, w := range words {
		if w != "" {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

func listIcons(templatesFS fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(templatesFS, "icons")
	if err != nil {
		return nil, err
	}
	icons := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".svg") {
			icons = append(icons, e.Name())
		}
	}
	sort.Strings(icons)
	return icons, nil
}

func dotClass(status string) string {
	switch status {
	case "healthy":
		return "green"
	case "degraded":
		return "red"
	default:
		return "yellow"
	}
}

func dotLabel(status string) string {
	switch status {
	case "healthy":
		return "Online"
	case "degraded":
		return "Offline"
	case "starting":
		return "Starting…"
	case "reloading":
		return "Reloading…"
	default:
		return status
	}
}

func New(templatesFS fs.FS, cfg *config.Manager, adminEvents chan<- hub.AdminEvent, logger *log.Logger, version string) (*Handler, error) {
	tmpl, err := template.New("admin").Funcs(templateFuncs).ParseFS(templatesFS, "*.html.tmpl")
	if err != nil {
		return nil, err
	}
	icons, err := listIcons(templatesFS)
	if err != nil {
		return nil, err
	}
	return &Handler{cfg: cfg, adminEvents: adminEvents, log: logger, tmpl: tmpl, icons: icons, version: version}, nil
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin", noStore(h.handleIndex))
	mux.HandleFunc("POST /admin/sources/save", noStore(h.handleSourcesSave))
	mux.HandleFunc("POST /admin/sources/reset", noStore(h.handleSourcesReset))
	mux.HandleFunc("GET /admin/sources", noStore(h.handleSourcesView))
	mux.HandleFunc("POST /admin/layouts", noStore(h.handleLayoutCreate))
	mux.HandleFunc("POST /admin/layouts/{id}/rename", noStore(h.handleLayoutRename))
	mux.HandleFunc("POST /admin/layouts/{id}/delete", noStore(h.handleLayoutDelete))
	mux.HandleFunc("POST /admin/sections", noStore(h.handleSectionAdd))
	mux.HandleFunc("POST /admin/sections/{id}/delete", noStore(h.handleSectionDelete))
	mux.HandleFunc("POST /admin/auxes", noStore(h.handleAuxesSave))
	mux.HandleFunc("POST /admin/auxes/send-on", noStore(h.handleAuxSendOn))
	mux.HandleFunc("GET /admin/auxes/send-on/reset", noStore(h.handleAuxSendOnReset))
	mux.HandleFunc("POST /admin/network", noStore(h.handleNetworkUpdate))
	mux.HandleFunc("GET /admin/network/status", noStore(h.handleNetworkStatus))
	mux.HandleFunc("POST /admin/settings", noStore(h.handleSettingsUpdate))
	mux.HandleFunc("POST /admin/lock", noStore(h.handleLockUpdate))
	mux.HandleFunc("POST /admin/external-devices", noStore(h.handleExternalDeviceAdd))
	mux.HandleFunc("POST /admin/external-devices/{id}", noStore(h.handleExternalDeviceUpdate))
	mux.HandleFunc("POST /admin/external-devices/{id}/delete", noStore(h.handleExternalDeviceDelete))
	mux.HandleFunc("POST /admin/presets/save", noStore(h.handlePresetSave))
	mux.HandleFunc("POST /admin/presets/reset", noStore(h.handlePresetsReset))
	mux.HandleFunc("POST /admin/presets/upload", noStore(h.handlePresetsUpload))
	mux.HandleFunc("GET /admin/presets/download", noStore(h.handlePresetsDownloadAll))
	mux.HandleFunc("GET /admin/presets/{uuid}/download", noStore(h.handlePresetDownload))
	mux.HandleFunc("POST /admin/presets/{uuid}/load", noStore(h.handlePresetLoad))
	mux.HandleFunc("POST /admin/presets/{uuid}/update", noStore(h.handlePresetUpdate))
	mux.HandleFunc("POST /admin/presets/{uuid}/delete", noStore(h.handlePresetDelete))
}

func noStore(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		fn(w, r)
	}
}

func (h *Handler) queryConfig(ctx context.Context) (oscmsg.ConfigPayload, error) {
	reply := make(chan oscmsg.ConfigPayload, 1)
	select {
	case h.adminEvents <- hub.AdminEvent{Kind: hub.AdminQuery, Reply: reply}:
	case <-ctx.Done():
		return oscmsg.ConfigPayload{}, ctx.Err()
	}
	select {
	case cfg := <-reply:
		return cfg, nil
	case <-ctx.Done():
		return oscmsg.ConfigPayload{}, ctx.Err()
	}
}

func (h *Handler) queryStatus(ctx context.Context) (hub.DeskStatusInfo, error) {
	reply := make(chan hub.DeskStatusInfo, 1)
	select {
	case h.adminEvents <- hub.AdminEvent{Kind: hub.AdminStatusQuery, StatusReply: reply}:
	case <-ctx.Done():
		return hub.DeskStatusInfo{}, ctx.Err()
	}
	select {
	case status := <-reply:
		return status, nil
	case <-ctx.Done():
		return hub.DeskStatusInfo{}, ctx.Err()
	}
}

func (h *Handler) saveLayout(ctx context.Context, layoutID string, items []config.LayoutItem, auxes []config.Aux) (oscmsg.ConfigPayload, error) {
	state, err := h.cfg.LoadStateOrDefault()
	if err != nil {
		return oscmsg.ConfigPayload{}, err
	}
	nl, ok := state.MixerLayouts[layoutID]
	if !ok {

		layoutID = config.DefaultLayoutID
		nl = state.MixerLayouts[config.DefaultLayoutID]
	}
	nl.Items = items
	state.MixerLayouts[layoutID] = nl
	state.Auxes = auxes
	if err := h.cfg.SaveState(state); err != nil {
		return oscmsg.ConfigPayload{}, err
	}
	h.applyState(state)
	return h.queryConfig(ctx)
}

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

func (h *Handler) applyState(state config.State) {
	global, err := h.cfg.LoadGlobalOrDefault()
	var devices []hub.ExternalDevice
	var autoHideChannel, autoHideGroup, autoHideAux []string
	var debug bool
	if err != nil {
		h.log.Printf("admin apply: reload global config: %v", err)
	} else {
		devices = ResolveExternalDevices(global.ExternalDevices, h.log, global.Debug)
		autoHideChannel = global.AutoHideChannelPatterns
		autoHideGroup = global.AutoHideGroupPatterns
		autoHideAux = global.AutoHideAuxPatterns
		debug = global.Debug
	}

	select {
	case h.adminEvents <- hub.AdminEvent{
		Kind: hub.AdminApply, Layouts: state.MixerLayouts, Auxes: state.Auxes,
		ExternalDevices:         devices,
		AutoHideChannelPatterns: autoHideChannel,
		AutoHideGroupPatterns:   autoHideGroup,
		AutoHideAuxPatterns:     autoHideAux,
		Debug:                   debug,
	}:
	default:

		h.log.Printf("admin apply: event channel full, change saved to disk but not yet applied live")
	}
}

func ResolveExternalDevices(devices []config.ExternalDevice, logger *log.Logger, debug bool) []hub.ExternalDevice {
	out := make([]hub.ExternalDevice, 0, len(devices))
	for _, d := range devices {
		ip := net.ParseIP(d.IP)
		if ip == nil {
			logger.Printf("external device %q: invalid IP %q, skipping", d.Name, d.IP)
			continue
		}
		out = append(out, hub.ExternalDevice{
			Name: d.Name, Addr: &net.UDPAddr{IP: ip, Port: d.Port},
			Broadcast: d.Broadcast, Loopback: d.Loopback,
		})
		if debug {
			logger.Printf("external device %q: relaying to %s:%d (broadcast=%v loopback=%v)",
				d.Name, ip, d.Port, d.Broadcast, d.Loopback)
		}
	}
	return out
}

func backfillExternalDeviceIDs(global *config.Global) bool {
	changed := false
	for i := range global.ExternalDevices {
		if global.ExternalDevices[i].ID == "" {
			id, err := config.NewID()
			if err != nil {
				continue
			}
			global.ExternalDevices[i].ID = id
			changed = true
		}
	}
	return changed
}
