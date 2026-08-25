// Package admin is the htmx-driven, server-rendered admin panel: GET
// /admin renders the full page (web/admin/layout.html.tmpl composing five
// section partials); every POST responds with just its own section's
// re-rendered fragment, swapped in place by htmx (hx-post/hx-target/
// hx-swap on each form/button — see web/admin/*.html.tmpl). Sources are
// reordered by real drag-and-drop (SortableJS, vendored — see
// web/admin/admin.js), which POSTs the full new ordering to
// /admin/sources/reorder rather than going through htmx's own form
// machinery, since the drag gesture isn't a form submission.
//
// A preset load changes both the sources and auxes sections at once, but
// only one can be the request's actual hx-target — the other two updates
// ride along as htmx out-of-band swaps in the same response (see
// SourcesSectionData/AuxesSectionData's OOB field and handlePresetLoad).
//
// SCOPE NOTE: per the earlier explicit decision, desk/server/OSC network
// settings are save-to-disk-and-require-restart. Source/aux edits and
// presets apply live via hub.AdminEvent — no restart, no client disconnect.
package admin

import (
	"context"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
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
}

// New builds a Handler, parsing admin templates from the given filesystem
// (web.AdminFS, rooted at "admin" via fs.Sub — see web/assets.go).
func New(templatesFS fs.FS, cfg *config.Manager, adminEvents chan<- hub.AdminEvent, logger *log.Logger) (*Handler, error) {
	tmpl, err := template.ParseFS(templatesFS, "*.html.tmpl")
	if err != nil {
		return nil, err
	}
	return &Handler{cfg: cfg, adminEvents: adminEvents, log: logger, tmpl: tmpl}, nil
}

// Register mounts the admin routes onto mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin", h.handleIndex)
	mux.HandleFunc("POST /admin/sources/{type}/{number}", h.handleSourceUpdate)
	mux.HandleFunc("POST /admin/sources/reorder", h.handleSourcesReorder)
	mux.HandleFunc("POST /admin/auxes/{number}", h.handleAuxUpdate)
	mux.HandleFunc("POST /admin/network", h.handleNetworkUpdate)
	mux.HandleFunc("POST /admin/external-devices", h.handleExternalDeviceAdd)
	mux.HandleFunc("POST /admin/external-devices/{name}/delete", h.handleExternalDeviceDelete)
	mux.HandleFunc("POST /admin/presets/save", h.handlePresetSave)
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

// applyState pushes a freshly-saved state live into hub (no restart, no
// disconnect — see package doc). Re-reads global.json itself so external
// device edits and source/aux edits both flow through the same apply path.
func (h *Handler) applyState(state config.State) {
	global, err := h.cfg.LoadGlobalOrDefault()
	var devices []hub.ExternalDevice
	if err != nil {
		h.log.Printf("admin apply: reload global config: %v", err)
	} else {
		devices = ResolveExternalDevices(global.ExternalDevices, h.log)
	}

	select {
	case h.adminEvents <- hub.AdminEvent{
		Kind: hub.AdminApply, Sources: state.Sources, Auxes: state.Auxes, ExternalDevices: devices,
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
// not just from applyState.
func ResolveExternalDevices(devices []config.ExternalDevice, logger *log.Logger) []hub.ExternalDevice {
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
		logger.Printf("external device %q: relaying to %s:%d (broadcast=%v loopback=%v)",
			d.Name, ip, d.Port, d.Broadcast, d.Loopback)
	}
	return out
}
