// oscwebmixer is the composition root. Phase 3 scope: config persistence
// (config/global.json, config/state.json, config/presets.json) and the
// admin panel, wired live into the hub for source/aux/preset/external-
// device edits — see internal/admin's package doc for the scope note on
// what applies live vs. requires a restart. Phase 4 is built: session-reload
// (hub.handleSessionChange) and desk-timeout/Degraded (hub.checkDeskHealth)
// both live in internal/hub rather than a separate internal/health package
// — see checkDeskHealth's doc comment for why a second ticker case in hub's
// existing single-goroutine select loop covers it without a new goroutine.
package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mederham/oscwebmixer-go/internal/admin"
	"github.com/mederham/oscwebmixer-go/internal/config"
	"github.com/mederham/oscwebmixer-go/internal/desk"
	"github.com/mederham/oscwebmixer-go/internal/hub"
	"github.com/mederham/oscwebmixer-go/internal/wsserver"
	"github.com/mederham/oscwebmixer-go/web"
)

func main() {
	// Every network flag below only overrides config/global.json when
	// explicitly passed on the command line (checked via flag.Visit) —
	// config is the normal source of truth once it exists; these flags
	// exist mainly for the kind of ad hoc real-desk testing this project
	// has leaned on heavily so far.
	deskIPFlag := flag.String("desk-ip", "", "override desk IP")
	deskPortFlag := flag.Int("desk-port", 0, "override desk port")
	listenPortFlag := flag.Int("listen-port", 0, "override local OSC listen port")
	httpPortFlag := flag.Int("http-port", 0, "override HTTP server port")
	configDir := flag.String("config-dir", "config", "directory for global.json/state.json/presets.json")
	runFor := flag.Duration("run-for", 0, "exit automatically after this long (0 = run until interrupted)")
	flag.Parse()

	logger := log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds)

	cfgManager := config.New(*configDir)
	if err := cfgManager.EnsureDir(); err != nil {
		logger.Fatalf("config dir: %v", err)
	}
	global, err := cfgManager.LoadGlobalOrDefault()
	if err != nil {
		logger.Fatalf("load global config: %v", err)
	}
	state, err := cfgManager.LoadStateOrDefault()
	if err != nil {
		logger.Fatalf("load state: %v", err)
	}

	provided := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { provided[f.Name] = true })
	if provided["desk-ip"] {
		global.Desk.IP = *deskIPFlag
	}
	if provided["desk-port"] {
		global.Desk.Port = *deskPortFlag
	}
	if provided["listen-port"] {
		global.OSC.Port = *listenPortFlag
	}
	if provided["http-port"] {
		global.Server.Port = *httpPortFlag
	}
	if global.Desk.IP == "" {
		logger.Fatalf("no desk IP configured — set desk.ip in %s/global.json or pass -desk-ip", *configDir)
	}

	// Persist the resolved config immediately (even if it only came from
	// CLI flags on a first run) so global.json — and therefore the admin
	// panel's Network form — always reflects what's actually running.
	// Without this, a CLI-flag-only desk IP would show as a blank field in
	// admin, and hitting Save there would silently wipe it on next restart.
	if err := cfgManager.SaveGlobal(global); err != nil {
		logger.Printf("warning: could not persist resolved config: %v", err)
	}

	conn, err := desk.Listen(global.OSC.Port, global.Desk.IP, global.Desk.Port)
	if err != nil {
		logger.Fatalf("listen: %v", err)
	}
	defer conn.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *runFor > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *runFor)
		defer cancel()
	}

	h := hub.New(conn, nil, logger, hub.Options{
		DeskIP:                  global.Desk.IP,
		StartupRetryInterval:    2 * time.Second,
		HealthCheckInterval:     3 * time.Second,
		InitialLayout:           state.MixerLayout,
		InitialAuxes:            state.Auxes,
		ExternalDevices:         admin.ResolveExternalDevices(global.ExternalDevices, logger),
		AutoHideChannelPatterns: global.AutoHideChannelPatterns,
		AutoHideGroupPatterns:   global.AutoHideGroupPatterns,
		AutoHideAuxPatterns:     global.AutoHideAuxPatterns,
		Debug:                   global.Debug,
	})

	clientEvents := make(chan hub.ClientEvent, 64)
	adminEvents := make(chan hub.AdminEvent, 16)
	ws := wsserver.New(clientEvents, logger)

	adminTemplatesFS, err := fs.Sub(web.AdminFS, "admin")
	if err != nil {
		logger.Fatalf("admin assets: %v", err)
	}
	adminHandler, err := admin.New(adminTemplatesFS, cfgManager, adminEvents, logger)
	if err != nil {
		logger.Fatalf("admin templates: %v", err)
	}

	mixerFS, err := fs.Sub(web.MixerFS, "mixer")
	if err != nil {
		logger.Fatalf("mixer assets: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/ws", ws)
	adminHandler.Register(mux)
	// Static assets under /admin/ (admin.css, the vendored htmx.min.js/
	// sortable.min.js, admin.js) — a subtree pattern, so it coexists with
	// the exact "GET /admin" route above without conflict, and with the
	// POST routes admin.Register() added (different method, different
	// match space entirely).
	mux.Handle("GET /admin/", http.StripPrefix("/admin/", http.FileServerFS(adminTemplatesFS)))
	mux.Handle("/", http.FileServer(http.FS(mixerFS)))

	httpServer := &http.Server{Addr: fmt.Sprintf(":%d", global.Server.Port), Handler: mux}
	go func() {
		logger.Printf("HTTP/mixer server listening on :%d (admin at /admin)", global.Server.Port)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Printf("http server: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		_ = httpServer.Close()
	}()

	go func() {
		select {
		case snap := <-h.Ready():
			logger.Printf("READY: %d channels, %d auxes, %d groups", snap.Channels, snap.Auxes, snap.Groups)
		case <-ctx.Done():
		}
	}()

	deskErr := make(chan error, 1)
	go func() { deskErr <- conn.Run(ctx) }()

	logger.Printf("connecting to desk %s:%d, listening on :%d", global.Desk.IP, global.Desk.Port, global.OSC.Port)
	err = h.Run(ctx, conn.Inbound(), clientEvents, adminEvents)
	<-deskErr // wait for the reader goroutine to unwind too

	if err != nil && err != context.Canceled && err != context.DeadlineExceeded {
		logger.Fatalf("hub: %v", err)
	}
	logger.Printf("shutting down (%v)", err)
}
