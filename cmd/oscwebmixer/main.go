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

const defaultHTTPPort = 80

var version = "dev"

func main() {
	deskIPFlag := flag.String("desk-ip", "", "override desk IP")
	deskPortFlag := flag.Int("desk-port", 0, "override desk port")
	oscPortFlag := flag.Int("osc-port", 0, "override local OSC listen port")
	configDir := flag.String("config-dir", "config", "directory for global.json/state.json/presets.json")
	runFor := flag.Duration("run-for", 0, "exit automatically after this long (0 = run until interrupted)")
	versionFlag := flag.Bool("version", false, "print version and exit")
	healthcheckFlag := flag.Bool("healthcheck", false, "check that this instance's own HTTP server is responding, exit 0/1 accordingly (for Docker HEALTHCHECK — scratch has no shell/curl to run one externally, see Dockerfile)")
	flag.Parse()

	if *versionFlag {
		fmt.Println(version)
		return
	}

	httpPort := defaultHTTPPort
	if v, ok, err := envInt("OSCWEBMIXER_HTTP_PORT"); ok {
		if err != nil {
			fmt.Fprintf(os.Stderr, "OSCWEBMIXER_HTTP_PORT: %v\n", err)
			os.Exit(1)
		}
		httpPort = v
	}

	if *healthcheckFlag {
		got, err := healthcheck(httpPort)
		if err != nil {
			fmt.Fprintln(os.Stderr, "healthcheck:", err)
			os.Exit(1)
		}
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
	if v, ok := resolveString(provided["desk-ip"], *deskIPFlag, "OSCWEBMIXER_DESK_IP"); ok {
		global.Desk.IP = v
	}
	if v, ok := resolveInt(provided["desk-port"], *deskPortFlag, "OSCWEBMIXER_DESK_PORT", logger); ok {
		global.Desk.Port = v
	}
	if v, ok := resolveInt(provided["osc-port"], *oscPortFlag, "OSCWEBMIXER_OSC_PORT", logger); ok {
		global.OSC.Port = v
	}

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
		TrustedDeskIPs:          envStringList("OSCWEBMIXER_DESK_TRUSTED_IPS"),
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
	<-deskErr

	if err != nil && err != context.Canceled && err != context.DeadlineExceeded {
		logger.Fatalf("hub: %v", err)
	}
	logger.Printf("shutting down (%v)", err)
}

func envInt(key string) (val int, ok bool, err error) {
	v := os.Getenv(key)
	if v == "" {
		return 0, false, nil
	}
	n, err := strconv.Atoi(v)
	return n, true, err
}

func envStringList(key string) []string {
	v := os.Getenv(key)
	if v == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func resolveString(provided bool, flagVal string, envKey string) (string, bool) {
	if provided {
		return flagVal, true
	}
	if v := os.Getenv(envKey); v != "" {
		return v, true
	}
	return "", false
}

func resolveInt(provided bool, flagVal int, envKey string, logger *log.Logger) (int, bool) {
	if provided {
		return flagVal, true
	}
	if v, ok, err := envInt(envKey); ok {
		if err != nil {
			logger.Fatalf("%s: %v", envKey, err)
		}
		return v, true
	}
	return 0, false
}

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
