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
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mederham/oscwebmixer-go/internal/admin"
	"github.com/mederham/oscwebmixer-go/internal/config"
	"github.com/mederham/oscwebmixer-go/internal/desk"
	"github.com/mederham/oscwebmixer-go/internal/hub"
	"github.com/mederham/oscwebmixer-go/internal/wsserver"
	"github.com/mederham/oscwebmixer-go/web"
)

// defaultHTTPPort is used when OSCWEBMIXER_HTTP_PORT isn't set — matches
// the port config.Global's old, now-removed Server field used to default
// to for a fresh install.
const defaultHTTPPort = 80

// version is set at build time via -ldflags "-X main.version=X.Y.Z" (see
// the VERSION file and Dockerfile/build_router_image_arm64.sh, its two
// consumers) — "dev" is what a plain `go build`/`go run` without that flag
// gets, so a local build is always visibly distinguishable from a release.
var version = "dev"

func main() {
	// Every network flag below only overrides config/global.json when
	// explicitly passed on the command line (checked via flag.Visit) —
	// config is the normal source of truth once it exists; these flags
	// exist mainly for the kind of ad hoc real-desk testing this project
	// has leaned on heavily so far.
	deskIPFlag := flag.String("desk-ip", "", "override desk IP")
	deskPortFlag := flag.Int("desk-port", 0, "override desk port")
	listenPortFlag := flag.Int("listen-port", 0, "override local OSC listen port")
	configDir := flag.String("config-dir", "config", "directory for global.json/state.json/presets.json")
	runFor := flag.Duration("run-for", 0, "exit automatically after this long (0 = run until interrupted)")
	versionFlag := flag.Bool("version", false, "print version and exit")
	healthcheckFlag := flag.Bool("healthcheck", false, "check that this instance's own HTTP server is responding, exit 0/1 accordingly (for Docker HEALTHCHECK — scratch has no shell/curl to run one externally, see Dockerfile)")
	flag.Parse()

	if *versionFlag {
		fmt.Println(version)
		return
	}

	// Resolved before the healthcheck flag below, and independently of
	// cfgManager/logger — a healthcheck runs frequently (every
	// HEALTHCHECK --interval) and has no business touching config files
	// or a log stream, just a fast local HTTP round trip.
	//
	// The HTTP server port isn't part of config.Global at all (see its doc
	// comment) — admin can't edit it (rebinding the listener serving the
	// very request that changed it isn't something to support), so
	// there's nothing to persist and nothing that can go stale on disk:
	// env var or this hardcoded default is the only source of truth,
	// resolved fresh on every start.
	httpPort := defaultHTTPPort
	if v := os.Getenv("OSCWEBMIXER_HTTP_PORT"); v != "" {
		port, err := strconv.Atoi(v)
		if err != nil {
			fmt.Fprintf(os.Stderr, "OSCWEBMIXER_HTTP_PORT=%q: %v\n", v, err)
			os.Exit(1)
		}
		httpPort = port
	}

	if *healthcheckFlag {
		got, err := healthcheck(httpPort)
		if err != nil {
			fmt.Fprintln(os.Stderr, "healthcheck:", err)
			os.Exit(1)
		}
		// Printed even on success: this is scratch, no shell to exec into
		// and manually curl /version for a live look — `docker inspect`'s
		// Health.Log (stdout+stderr of the last few runs) is the only
		// place any of this is ever visible.
		fmt.Println("ok:", got)
		return
	}

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
	// No desk IP is a normal starting state, not an error — the router
	// container is expected to boot with none set and get pointed at a
	// desk later via the admin panel's Network tab (see desk.Listen).

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
		DeskPort:                global.Desk.Port,
		OSCPort:                 global.OSC.Port,
		StartupRetryInterval:    2 * time.Second,
		HealthCheckInterval:     3 * time.Second,
		InitialLayouts:          state.MixerLayouts,
		InitialAuxes:            state.Auxes,
		ExternalDevices:         admin.ResolveExternalDevices(global.ExternalDevices, logger, global.Debug),
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
	adminHandler, err := admin.New(adminTemplatesFS, cfgManager, adminEvents, logger, version)
	if err != nil {
		logger.Fatalf("admin templates: %v", err)
	}

	mixerFS, err := fs.Sub(web.MixerFS, "mixer")
	if err != nil {
		logger.Fatalf("mixer assets: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/ws", ws)
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, version)
	})
	adminHandler.Register(mux)
	// Static assets under /admin/ (admin.css, the vendored htmx.min.js/
	// sortable.min.js, admin.js) — a subtree pattern, so it coexists with
	// the exact "GET /admin" route above without conflict, and with the
	// POST routes admin.Register() added (different method, different
	// match space entirely).
	mux.Handle("GET /admin/", http.StripPrefix("/admin/", http.FileServerFS(adminTemplatesFS)))
	mux.Handle("/", http.FileServer(http.FS(mixerFS)))

	httpServer := &http.Server{Addr: fmt.Sprintf(":%d", httpPort), Handler: mux}
	go func() {
		logger.Printf("HTTP/mixer server listening on :%d (admin at /admin)", httpPort)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Printf("http server: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		_ = httpServer.Close()
	}()

	deskErr := make(chan error, 1)
	go func() { deskErr <- conn.Run(ctx) }()

	if global.Desk.IP == "" {
		logger.Printf("no desk configured yet (set it in the admin Network tab), listening on :%d", global.OSC.Port)
	} else {
		logger.Printf("connecting to desk %s:%d, listening on :%d", global.Desk.IP, global.Desk.Port, global.OSC.Port)
	}
	err = h.Run(ctx, conn.Inbound(), clientEvents, adminEvents)
	<-deskErr // wait for the reader goroutine to unwind too

	if err != nil && err != context.Canceled && err != context.DeadlineExceeded {
		logger.Fatalf("hub: %v", err)
	}
	logger.Printf("shutting down (%v)", err)
}

// healthcheck backs the --healthcheck flag: a plain HTTP GET against this
// same process's own /version endpoint. That endpoint requires nothing
// (no desk, no config) to answer, so this only proves the HTTP server
// itself is alive and serving — deliberately not tied to hub.Status
// (starting/degraded/reloading are all normal, expected states whenever
// the desk itself is rebooting or between sessions, not container-level
// failures a restart would fix).
func healthcheck(port int) (string, error) {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/version", port))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("/version returned %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}
