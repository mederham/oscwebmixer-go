package hub

import (
	"context"
	"fmt"
	"log"
	"net"
	"regexp"
	"sync"
	"time"

	"github.com/mederham/oscwebmixer-go/internal/config"
	"github.com/mederham/oscwebmixer-go/internal/desk"
	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
	"github.com/mederham/oscwebmixer-go/internal/pipeline"
)

type DeskSender interface {
	Send(msg oscmsg.Message) error
	SendTo(msg oscmsg.Message, addr *net.UDPAddr) error
	SetRemote(ip string, port int) error
	Rebind(localPort int) error
}

type ExternalDevice struct {
	Name      string
	Addr      *net.UDPAddr
	Broadcast bool
	Loopback  bool
}

type Status string

const (
	StatusStarting  Status = "starting"
	StatusHealthy   Status = "healthy"
	StatusDegraded  Status = "degraded"
	StatusReloading Status = "reloading"
)

const sessionChangeAddress = "/Console/Session/!"

const healthProbeAddress = "/Console/Name/?"

const degradedAfterMisses = 3

var cacheableAddresses = []*regexp.Regexp{
	regexp.MustCompile(`^/Input_Channels/(\d{1,3})/Aux_Send/(\d{1,3})/send_level$`),
	regexp.MustCompile(`^/Input_Channels/(\d{1,3})/Aux_Send/(\d{1,3})/send_pan$`),
	regexp.MustCompile(`^/Input_Channels/(\d{1,3})/Channel_Input/name$`),
	regexp.MustCompile(`^/Input_Channels/(\d{1,3})/Channel_Input/stereo_mode$`),
	regexp.MustCompile(`^/Aux_Outputs/(\d+)/Buss_Trim/name$`),
	regexp.MustCompile(`^/Console/Aux_Outputs/modes$`),
	regexp.MustCompile(`^/Console/Input_Channels$`),
	regexp.MustCompile(`^/Group_Outputs/(\d{1,3})/Aux_Send/(\d{1,3})/send_level$`),
	regexp.MustCompile(`^/Group_Outputs/(\d{1,3})/Aux_Send/(\d{1,3})/send_pan$`),
	regexp.MustCompile(`^/Group_Outputs/(\d+)/Buss_Trim/name$`),
	regexp.MustCompile(`^/Console/Group_Outputs/modes$`),
}

type Snapshot struct {
	Channels, Auxes, Groups int
}

type broadcastSource struct {
	ip       string
	clientID string
}

func deskSource(ip string) broadcastSource {
	return broadcastSource{ip: ip}
}

func clientSource(id string) broadcastSource {
	return broadcastSource{clientID: id}
}

type Hub struct {
	desk     DeskSender
	pipeline *pipeline.Pipeline
	log      *log.Logger

	deskIP         string
	deskPort       int
	oscPort        int
	trustedDeskIPs map[string]bool

	cache  map[string]oscmsg.Message
	status Status

	channelCount int
	auxCount     int
	groupCount   int

	snapshotRequested bool
	currentSnapshot   int
	snapshotName      string

	clients         map[string]chan<- oscmsg.Envelope
	dropStreak      map[string]int
	externalDevices []ExternalDevice

	lastSendErrLogged map[string]time.Time

	lastClientDriven map[string]time.Time

	layouts      map[string]config.NamedLayout
	auxOverrides []config.Aux

	autoHideChannelPatterns []string
	autoHideGroupPatterns   []string
	autoHideAuxPatterns     []string

	startupRetryInterval time.Duration

	pendingQuery      string
	pendingQuerySince time.Time

	lastDeskSeen        time.Time
	healthCheckInterval time.Duration

	ready     chan Snapshot
	readyOnce sync.Once

	debug bool

	locked bool
}

func (h *Hub) isDeskSource(ip string) bool {
	return ip == h.deskIP || h.trustedDeskIPs[ip]
}

func (h *Hub) debugf(format string, args ...any) {
	if h.debug {
		h.log.Printf("[debug] "+format, args...)
	}
}

type Options struct {
	DeskIP   string
	DeskPort int
	OSCPort  int

	// List of additional IPs to count as console source IPs (e.g. when using NAT)
	TrustedDeskIPs []string

	StartupRetryInterval time.Duration

	HealthCheckInterval time.Duration

	InitialLayouts map[string]config.NamedLayout
	InitialAuxes   []config.Aux

	ExternalDevices []ExternalDevice

	AutoHideChannelPatterns []string
	AutoHideGroupPatterns   []string
	AutoHideAuxPatterns     []string

	Debug bool
}

func New(d DeskSender, pipe *pipeline.Pipeline, logger *log.Logger, opts Options) *Hub {
	healthCheckInterval := opts.HealthCheckInterval
	if healthCheckInterval <= 0 {
		healthCheckInterval = 24 * time.Hour
	}
	trustedDeskIPs := make(map[string]bool, len(opts.TrustedDeskIPs))
	for _, ip := range opts.TrustedDeskIPs {
		trustedDeskIPs[ip] = true
	}
	return &Hub{
		desk:                    d,
		pipeline:                pipe,
		log:                     logger,
		deskIP:                  opts.DeskIP,
		deskPort:                opts.DeskPort,
		oscPort:                 opts.OSCPort,
		trustedDeskIPs:          trustedDeskIPs,
		cache:                   make(map[string]oscmsg.Message),
		status:                  StatusStarting,
		ready:                   make(chan Snapshot, 1),
		clients:                 make(map[string]chan<- oscmsg.Envelope),
		dropStreak:              make(map[string]int),
		lastSendErrLogged:       make(map[string]time.Time),
		lastClientDriven:        make(map[string]time.Time),
		layouts:                 opts.InitialLayouts,
		auxOverrides:            opts.InitialAuxes,
		autoHideChannelPatterns: opts.AutoHideChannelPatterns,
		autoHideGroupPatterns:   opts.AutoHideGroupPatterns,
		autoHideAuxPatterns:     opts.AutoHideAuxPatterns,
		externalDevices:         opts.ExternalDevices,
		startupRetryInterval:    opts.StartupRetryInterval,
		healthCheckInterval:     healthCheckInterval,
		debug:                   opts.Debug,
	}
}

func (h *Hub) Ready() <-chan Snapshot { return h.ready }

func (h *Hub) Run(ctx context.Context, deskInbound <-chan desk.InboundMessage, clientEvents <-chan ClientEvent, adminEvents <-chan AdminEvent) error {
	h.advanceStartup()

	retry := time.NewTicker(h.startupRetryInterval)
	defer retry.Stop()

	health := time.NewTicker(h.healthCheckInterval)
	defer health.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case in, ok := <-deskInbound:
			if !ok {
				return nil
			}
			fromIP := ""
			if in.From != nil {
				fromIP = in.From.IP.String()
			}
			h.handleDeskMessage(in.Message, fromIP)
		case ev, ok := <-clientEvents:
			if !ok {
				return nil
			}
			h.handleClientEvent(ev)
		case ev, ok := <-adminEvents:
			if !ok {
				return nil
			}
			h.handleAdminEvent(ev)
		case <-retry.C:
			if h.status == StatusStarting || h.status == StatusReloading {
				h.advanceStartup()
			}
		case <-health.C:
			h.checkDeskHealth()
		}
	}
}

func (h *Hub) checkDeskHealth() {
	if h.status == StatusHealthy || h.status == StatusDegraded {
		if h.status == StatusHealthy && time.Since(h.lastDeskSeen) > h.healthCheckInterval*degradedAfterMisses {
			h.status = StatusDegraded
			h.log.Printf("desk health: degraded (no traffic for %s)", time.Since(h.lastDeskSeen).Round(time.Second))
			h.pushStatus()
		}
		h.query(healthProbeAddress)
	}
}

func (h *Hub) pushStatus() {
	status := oscmsg.NewStatusEnvelope(string(h.status), h.locked)
	for id, out := range h.clients {
		h.sendTo(id, out, status)
	}
}

func (h *Hub) pushStatusAndConfig() {
	status := oscmsg.NewStatusEnvelope(string(h.status), h.locked)
	cfg := oscmsg.NewConfigEnvelope(h.buildConfig())
	for id, out := range h.clients {
		h.sendTo(id, out, status)
		h.sendTo(id, out, cfg)
	}
}

func (h *Hub) handleDeskMessage(msg oscmsg.Message, fromIP string) {
	if h.debug {
		h.debugf("desk-path %s from %s", msg.Address, fromIP)
	}

	if !h.isDeskSource(fromIP) {
		var device *ExternalDevice
		for i := range h.externalDevices {
			if h.externalDevices[i].Addr.IP.String() == fromIP {
				device = &h.externalDevices[i]
				break
			}
		}
		if device == nil {
			if h.debug {
				h.debugf("  -> from %s: dropped (unrecognized source)", fromIP)
			}
			return
		}
		if !device.Broadcast {
			if h.debug {
				h.debugf("  -> from external %q: dropped (broadcast disabled)", device.Name)
			}
			return
		}
	}

	if h.isDeskSource(fromIP) {
		h.lastDeskSeen = time.Now()
		if h.status == StatusDegraded {
			h.status = StatusHealthy
			h.log.Printf("desk health: recovered")
			h.pushStatusAndConfig()
		}
	}

	out, ok := h.pipeline.Run(msg)
	if !ok {
		h.log.Printf("pipeline vetoed %s", msg.Address)
		return
	}
	msg = out

	if msg.Address == sessionChangeAddress {
		h.handleSessionChange()
	}

	h.handleSnapshotMessage(msg)

	if h.maybeCache(msg) && h.debug {
		h.debugf("cached %s %v", msg.Address, msg.Args)
	}

	if h.status == StatusStarting || h.status == StatusReloading {
		if h.debug {
			h.debugf("  status=%s: not broadcasting, still in startup sequence", h.status)
		}
		h.advanceStartup()
		return
	}

	h.broadcast(msg, deskSource(fromIP))
}

func (h *Hub) handleSessionChange() {
	h.log.Printf("session change: reloading")

	h.cache = make(map[string]oscmsg.Message)
	h.channelCount, h.auxCount, h.groupCount = 0, 0, 0
	h.snapshotRequested = false
	h.snapshotName = ""
	h.pendingQuery = ""
	h.status = StatusReloading
	h.pushStatus()
}

func (h *Hub) maybeCache(msg oscmsg.Message) bool {
	for _, re := range cacheableAddresses {
		if re.MatchString(msg.Address) {
			h.cache[msg.Address] = msg
			return true
		}
	}
	return false
}

func (h *Hub) advanceStartup() {
	channelCountMsg, ok := h.cache["/Console/Input_Channels"]
	if !ok {
		h.queryOnce("/Console/Input_Channels", "/Console/Channels/?")
		return
	}
	h.channelCount = intArg(channelCountMsg, 0)

	auxModes, ok := h.cache["/Console/Aux_Outputs/modes"]
	if !ok {
		h.queryOnce("/Console/Aux_Outputs/modes", "/Console/Aux_Outputs/modes/?")
		return
	}
	h.auxCount = len(auxModes.Args)

	groupModes, ok := h.cache["/Console/Group_Outputs/modes"]
	if !ok {
		h.queryOnce("/Console/Group_Outputs/modes", "/Console/Group_Outputs/modes/?")
		return
	}
	h.groupCount = len(groupModes.Args)

	for i := 1; i <= h.auxCount; i++ {
		key := fmt.Sprintf("/Aux_Outputs/%d/Buss_Trim/name", i)
		if _, ok := h.cache[key]; !ok {
			h.queryOnce(key, key+"/?")
			return
		}
	}

	for i := 1; i <= h.groupCount; i++ {
		key := fmt.Sprintf("/Group_Outputs/%d/Buss_Trim/name", i)
		if _, ok := h.cache[key]; !ok {
			h.queryOnce(key, key+"/?")
			return
		}
	}

	for i := 1; i <= h.channelCount; i++ {
		key := fmt.Sprintf("/Input_Channels/%d/Channel_Input/name", i)
		if _, ok := h.cache[key]; !ok {
			h.queryOnce(key, key+"/?")
			return
		}
	}

	for i := 1; i <= h.channelCount; i++ {
		key := fmt.Sprintf("/Input_Channels/%d/Channel_Input/stereo_mode", i)
		if _, ok := h.cache[key]; !ok {
			h.queryOnce(key, key+"/?")
			return
		}
	}

	if !h.snapshotRequested {
		h.snapshotRequested = true
		h.query("/Snapshots/Current_Snapshot/?")
	}

	if h.status != StatusHealthy {
		h.status = StatusHealthy
		h.log.Printf("startup complete: %d channels, %d auxes, %d groups", h.channelCount, h.auxCount, h.groupCount)
		h.readyOnce.Do(func() {
			h.ready <- Snapshot{Channels: h.channelCount, Auxes: h.auxCount, Groups: h.groupCount}
		})

		h.pushStatusAndConfig()
	}
}

func (h *Hub) query(address string) {
	if err := h.desk.Send(oscmsg.Message{Address: address, Args: []any{}}); err != nil {
		h.log.Printf("query %s: %v", address, err)
	}
}

func (h *Hub) queryOnce(cacheKey, address string) {
	if h.pendingQuery == cacheKey && time.Since(h.pendingQuerySince) < h.startupRetryInterval {
		return
	}
	h.pendingQuery = cacheKey
	h.pendingQuerySince = time.Now()
	h.query(address)
}

func intArg(msg oscmsg.Message, idx int) int {
	if idx >= len(msg.Args) {
		return 0
	}
	return argAsInt(msg.Args[idx])
}

func argAsInt(v any) int {
	switch x := v.(type) {
	case int32:
		return int(x)
	case float32:
		return int(x)
	default:
		return 0
	}
}

func argAsString(v any) string {
	s, _ := v.(string)
	return s
}
