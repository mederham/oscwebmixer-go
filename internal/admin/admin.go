// Package admin is the htmx-driven, server-rendered admin panel, organized
// into tabs (web/admin/layout.html.tmpl) over five section partials — GET
// /admin renders the full page; every POST responds with just its own
// section's re-rendered fragment, swapped in place by htmx (hx-post/
// hx-target/hx-swap on each form/button — see web/admin/*.html.tmpl).
//
// Sources is one flat, drag-and-drop-reorderable list of cards mixing
// sources and admin-created section markers (see oscmsg.LayoutItem's doc
// comment: display order and a source's section membership both come from
// list position, not a stored field). No Save button: reordering, the
// enabled toggle, and renaming a section all auto-submit the same htmx
// form (#sources-form, POST /admin/sources/save — see admin.js) the
// instant each happens. The form's one payload field is a JSON-encoded
// array built fresh from the live DOM by admin.js's htmx:configRequest
// listener, since the browser's native form-field serialization has no
// way to preserve an ordered, mixed-kind list — everything else about the
// request/response/swap lifecycle is still plain htmx, not a hand-rolled
// fetch.
//
// A preset load changes both the mixer layout and auxes at once, but only
// one can be the request's actual hx-target — the other rides along as an
// htmx out-of-band swap in the same response (see SourcesSectionData/
// AuxesSectionData's OOB field and handlePresetLoad).
//
// SCOPE NOTE: Desk IP/port and the OSC listen port apply live too (see
// AdminNetworkApply — desk.Conn.SetRemote/Rebind let hub reconfigure the
// UDP transport in place, no restart, same as everything else here). The
// HTTP server port is the one exception: it isn't editable from this panel
// at all, since rebinding the very listener serving the request that
// changed it isn't something to support — see OSCWEBMIXER_HTTP_PORT in
// main.go.
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

// Handler serves the admin panel.
type Handler struct {
	cfg         *config.Manager
	adminEvents chan<- hub.AdminEvent
	log         *log.Logger
	tmpl        *template.Template

	// icons is the aux icon picker's option list — read once at startup
	// from web/admin/icons/*.svg (embedded alongside the templates), not
	// hand-maintained as a list that could drift from the actual files.
	icons []string
}

// templateFuncs backs network.html.tmpl's "status-dot" fragment and the
// aux icon picker's option labels — the only places in this package that
// need to turn raw data (a hub.Status/timestamp, an icon filename) into
// display text, so a small FuncMap is simpler than Go-side view types just
// for these few fields.
var templateFuncs = template.FuncMap{
	"dotClass":     dotClass,
	"dotLabel":     dotLabel,
	"sinceRounded": func(t time.Time) string { return time.Since(t).Round(time.Second).String() },
	"iconLabel":    iconLabel,
	"badge":        badge, // "Ch 01" / "Grp 01" — derived from Type+Number, not stored (see SourceRow)
	"join":         strings.Join,
}

// iconLabel turns an icon filename ("acoustic-guitar.svg") into a
// human-readable option label ("Acoustic Guitar").
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

// listIcons enumerates the aux icon picker's available options.
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

// dotClass maps a hub.Status string to the status-dot's CSS color class.
// starting/reloading share yellow — both mean "desk is responding, hub is
// mid-sequence," not "desk is unreachable."
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

// New builds a Handler, parsing admin templates from the given filesystem
// (web.AdminFS, rooted at "admin" via fs.Sub — see web/assets.go).
func New(templatesFS fs.FS, cfg *config.Manager, adminEvents chan<- hub.AdminEvent, logger *log.Logger) (*Handler, error) {
	tmpl, err := template.New("admin").Funcs(templateFuncs).ParseFS(templatesFS, "*.html.tmpl")
	if err != nil {
		return nil, err
	}
	icons, err := listIcons(templatesFS)
	if err != nil {
		return nil, err
	}
	return &Handler{cfg: cfg, adminEvents: adminEvents, log: logger, tmpl: tmpl, icons: icons}, nil
}

// Register mounts the admin routes onto mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin", h.handleIndex)
	mux.HandleFunc("POST /admin/sources/save", h.handleSourcesSave)
	mux.HandleFunc("POST /admin/sources/reset", h.handleSourcesReset)
	mux.HandleFunc("POST /admin/sections", h.handleSectionAdd)
	mux.HandleFunc("POST /admin/sections/{id}/delete", h.handleSectionDelete)
	mux.HandleFunc("POST /admin/auxes", h.handleAuxesSave)
	mux.HandleFunc("POST /admin/network", h.handleNetworkUpdate)
	mux.HandleFunc("GET /admin/network/status", h.handleNetworkStatus)
	mux.HandleFunc("POST /admin/settings", h.handleSettingsUpdate)
	mux.HandleFunc("POST /admin/external-devices", h.handleExternalDeviceAdd)
	mux.HandleFunc("POST /admin/external-devices/{id}", h.handleExternalDeviceUpdate)
	mux.HandleFunc("POST /admin/external-devices/{id}/delete", h.handleExternalDeviceDelete)
	mux.HandleFunc("POST /admin/presets/save", h.handlePresetSave)
	mux.HandleFunc("POST /admin/presets/reset", h.handlePresetsReset)
	mux.HandleFunc("POST /admin/presets/{uuid}/load", h.handlePresetLoad)
	mux.HandleFunc("POST /admin/presets/{uuid}/delete", h.handlePresetDelete)
}

// queryConfig asks hub for its current buildConfig() output — the request/
// response-over-channel pattern the plan calls for, since hub state is
// only safe to read from the goroutine running Run.
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

// queryStatus asks hub for the live desk-connectivity snapshot the Network
// tab's status dot renders — same request/response-over-channel pattern as
// queryConfig, deliberately a separate query (see hub.DeskStatusInfo's doc
// comment for why this doesn't just ride along on ConfigPayload).
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

// saveState persists the mixer layout/auxes (each already reflecting
// whatever edit the caller made) in one atomic write, applies it live to
// hub, and returns a fresh post-apply ConfigPayload to render from — the
// same sequence every Sources/Auxes/Sections mutation follows.
func (h *Handler) saveState(ctx context.Context, layout []config.LayoutItem, auxes []config.Aux) (oscmsg.ConfigPayload, error) {
	state, err := h.cfg.LoadStateOrDefault()
	if err != nil {
		return oscmsg.ConfigPayload{}, err
	}
	state.MixerLayout = layout
	state.Auxes = auxes
	if err := h.cfg.SaveState(state); err != nil {
		return oscmsg.ConfigPayload{}, err
	}
	h.applyState(state)
	return h.queryConfig(ctx)
}

// applyState pushes a freshly-saved state live into hub (no restart, no
// disconnect — see package doc). Re-reads global.json itself so external
// device edits and source/aux edits both flow through the same apply path.
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
		Kind: hub.AdminApply, Layout: state.MixerLayout, Auxes: state.Auxes,
		ExternalDevices:         devices,
		AutoHideChannelPatterns: autoHideChannel,
		AutoHideGroupPatterns:   autoHideGroup,
		AutoHideAuxPatterns:     autoHideAux,
		Debug:                   debug,
	}:
	default:
		// The saved file is already correct regardless — this only means
		// the running process won't reflect it live until it next applies
		// a change or restarts. Channel is sized generously; this should
		// only ever fire under pathological load.
		h.log.Printf("admin apply: event channel full, change saved to disk but not yet applied live")
	}
}

// ResolveExternalDevices converts persisted config entries (IP as a
// string) into hub's runtime representation (a resolved *net.UDPAddr) —
// exported so main.go can reuse it for the initial hub.Options at startup,
// not just from applyState. debug gates the per-device "relaying to" line:
// applyState calls this on every admin save (Sources, Auxes, Settings,
// preset load — not just external-device edits), so logging it
// unconditionally would reprint the whole device list on saves that have
// nothing to do with external devices. The invalid-IP warning below is a
// real problem, not routine confirmation, so it always logs regardless.
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

// backfillExternalDeviceIDs assigns a fresh ID to any device saved before
// ExternalDevice gained one (a pre-upgrade global.json) — a one-time
// normalization, not a dedicated migration step, so every device is
// guaranteed to have a stable ID before handleExternalDeviceUpdate/Delete
// ever try to look one up. Reports whether it changed anything, so the
// caller only re-saves when needed.
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
