// Package hub is the single state-owning goroutine described in the plan:
// cache, startup sequencer, client registry, and (in later phases)
// desk-status state machine all live here, mutated only by the goroutine
// running Hub.Run — never touched directly from the desk-reader or
// WS-client goroutines. See plan: "Concurrency model."
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

// DeskSender is the subset of *desk.Conn the hub needs — kept as an
// interface so hub can be tested without a real UDP socket. SendTo exists
// for the external-device relay (see broadcast in client.go). SetRemote/
// Rebind let the admin panel apply a changed Desk IP/port or OSC listen
// port live, without a process restart — see handleAdminEvent's
// AdminNetworkApply case.
type DeskSender interface {
	Send(msg oscmsg.Message) error
	SendTo(msg oscmsg.Message, addr *net.UDPAddr) error
	SetRemote(ip string, port int) error
	Rebind(localPort int) error
}

// ExternalDevice mirrors one config/global.json externalDevices entry,
// with the IP:port already resolved — internal/admin does the
// parsing/DNS-adjacent work when loading or saving config, so hub never
// has to.
type ExternalDevice struct {
	Name      string
	Addr      *net.UDPAddr
	Broadcast bool
	Loopback  bool
}

// Status is the desk connectivity/session state (plan: "Healthcheck /
// connection-lifecycle redesign"). Reloading is reached via
// handleSessionChange, Degraded via checkDeskHealth/handleDeskMessage below.
type Status string

const (
	StatusStarting  Status = "starting"
	StatusHealthy   Status = "healthy"
	StatusDegraded  Status = "degraded"
	StatusReloading Status = "reloading"
)

// sessionChangeAddress is what the desk sends the instant a different
// session is loaded on the console — confirmed live (two captures): the
// desk then floods ~700 unprompted value updates, but never the topology
// (channel/aux/group counts, names, modes) a client needs — and the real
// iPad app itself explicitly re-queries that topology fresh rather than
// trusting anything from before the change. See handleSessionChange.
const sessionChangeAddress = "/Console/Session/!"

// healthProbeAddress is queried on every healthCheckInterval tick (plan:
// internal/health, "periodic /Console/Name/? ping") so silence during a
// genuinely idle desk (no fader moves, no meter flood) still gets a reply to
// judge liveness by — passively waiting for unprompted traffic alone would
// misreport a quiet-but-healthy desk as unresponsive.
const healthProbeAddress = "/Console/Name/?"

// degradedAfterMisses is how many consecutive healthCheckInterval ticks may
// pass with no desk traffic at all before declaring Degraded — one tick's
// worth of tolerance would false-positive on a single lost UDP reply to the
// probe above.
const degradedAfterMisses = 3

// cacheableAddresses mirrors the JS version's maybeCacheResponse whitelist
// (index.js), extended with the Group_Outputs equivalents confirmed live
// against the real desk in Phase 0 (/Console/Group_Outputs/modes,
// /Group_Outputs/N/Buss_Trim/name, /Group_Outputs/N/Aux_Send/M/send_level|
// send_pan — see probe output).
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

// Snapshot is a point-in-time readout of what the startup sequence learned,
// delivered over the Ready channel — not a live getter, since hub state is
// only safe to read from the goroutine running Run (see plan: later phases
// that need to query hub state from another goroutine, e.g. admin HTTP
// handlers, do it via request/response channels, not shared-memory reads).
type Snapshot struct {
	Channels, Auxes, Groups int
}

// broadcastSource identifies who a message must not be echoed back to,
// mirroring the JS version's info.address-based exclusion: for a
// desk-path message, IP carries the actual UDP sender address — which may
// be the desk, or an external device that also sends to our shared OSC
// socket (confirmed live: a real registered external device sending
// queries was, before this fix, always misidentified as "from the desk"
// and therefore silently never forwarded to the actual console). For a
// client-path message, ClientID is set instead and IP stays empty — an
// empty IP can never equal a real device's address, so the desk/
// external-device comparisons below naturally no-op for client-originated
// messages, matching the JS version's behavior of a WS object never
// equaling an IP string.
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

// Hub owns the OSC message cache, the startup sequencer, and the connected
// WS client registry. All fields below are touched only from the goroutine
// running Run.
type Hub struct {
	desk     DeskSender
	pipeline *pipeline.Pipeline
	log      *log.Logger

	// deskIP is the configured desk address, used to tell an actual desk
	// reply apart from a same-socket message that only looks like one
	// (see broadcastSource) — mirrors JS's config.desk.ip comparison.
	// deskPort/oscPort are tracked alongside it purely so
	// handleAdminEvent's AdminNetworkApply case can tell whether a saved
	// Network settings change actually changed anything, before touching
	// h.desk — neither is otherwise consulted by hub's own logic.
	deskIP   string
	deskPort int
	oscPort  int

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

	// lastSendErrLogged throttles broadcast's desk/external-device send-error
	// logging (see logSendErrThrottled) — keyed by "desk" or the external
	// device's address string. A destination that's down fails on literally
	// every message broadcast sends it (one dedicated socket per
	// destination — see desk.Conn.SendTo's doc comment — means that failure
	// no longer leaks onto other destinations, but it still fires at
	// message rate for its own), which would otherwise spam the log.
	lastSendErrLogged map[string]time.Time

	// lastClientDriven records, per OSC address, the last time a WS
	// client's own change to it was relayed — see
	// deskEchoSuppressWindow's doc comment for why broadcast uses this to
	// skip re-relaying the desk's echo of that same address to clients
	// shortly after.
	lastClientDriven map[string]time.Time

	// layout/auxOverrides are the admin-editable overlay loaded from
	// config/state.json — layered onto the live desk-reported data in
	// buildConfig(). layout is the ordered mixer layout (sources +
	// section markers — see config.LayoutItem's doc comment for why
	// array position is the single source of truth for both display
	// order and section membership). Updated live via AdminEvent, no
	// restart required (see Phase 3 scope note: only network settings
	// need a restart, everything here applies immediately and
	// re-broadcasts to connected clients).
	layout       []config.LayoutItem
	auxOverrides []config.Aux

	// autoHide{Channel,Group,Aux}Patterns mirror config.Global's fields of
	// the same name — see their doc comment. Updated live via AdminEvent,
	// same as layout/auxOverrides.
	autoHideChannelPatterns []string
	autoHideGroupPatterns   []string
	autoHideAuxPatterns     []string

	// startupRetryInterval: UDP is unreliable and a lost packet — our query
	// or the desk's reply — otherwise stalls the startup sequence forever,
	// since advanceStartup only re-runs reactively when another message
	// arrives. Confirmed live: the real desk startup stalled mid-sequence
	// on dropped packets on two separate runs. Run's ticker (below)
	// periodically re-invokes advanceStartup regardless of new traffic,
	// mirroring the JS version's fetchValues() 3s retry — but applied to
	// every step of the sequence, not just the first, since JS's version
	// of this same gap was only ever papered over by incidental unrelated
	// desk traffic keeping it moving.
	startupRetryInterval time.Duration

	// pendingQuery/pendingQuerySince track the one outstanding startup
	// query — see queryOnce's doc comment for why this exists.
	pendingQuery      string
	pendingQuerySince time.Time

	// lastDeskSeen/healthCheckInterval back the Healthy<->Degraded
	// transition — see checkDeskHealth. Left at its zero value until real
	// desk traffic actually arrives: the Degraded check only ever runs
	// once status is already Healthy, and status only becomes Healthy as
	// a direct result of desk traffic just having arrived (every step of
	// the startup sequence is driven by an inbound desk message, which
	// updates lastDeskSeen before that same message can advance startup)
	// — so the zero value is never read by that check, only by the admin
	// panel's "last seen" line, which uses it to show "never" for a desk
	// that hasn't actually been reachable yet (see AdminStatusQuery).
	lastDeskSeen        time.Time
	healthCheckInterval time.Duration

	ready     chan Snapshot
	readyOnce sync.Once

	// debug gates debugf — see Options.Debug.
	debug bool

	// locked is the admin-toggled personal-mixing kill switch (AdminSetLocked)
	// — deliberately runtime-only, not persisted to config/state.json: it's a
	// temporary "everyone hands off" switch for during a show, not a mixer
	// setting meant to survive a restart.
	locked bool
}

// debugf logs a per-hop trace line when Options.Debug is set — otherwise a
// no-op. Not gated behind h.log itself being non-nil since every Hub is
// required to have a logger (see New).
func (h *Hub) debugf(format string, args ...any) {
	if h.debug {
		h.log.Printf("[debug] "+format, args...)
	}
}

// Options bundles New's less-central parameters — the ones a caller
// usually either always specifies deliberately (StartupRetryInterval) or
// often leaves at the zero value (everything else, when there's no
// persisted config yet).
type Options struct {
	// DeskIP is the configured desk address — required for correctly
	// distinguishing an actual desk reply from a same-socket message that
	// only looks like one (see broadcastSource's doc comment). DeskPort/
	// OSCPort seed the change-detection fields of the same name — see
	// their doc comment on Hub.
	DeskIP   string
	DeskPort int
	OSCPort  int

	// StartupRetryInterval: how often the startup sequence re-sends its
	// current pending query if no reply has arrived. Pass a real-world
	// value like 2*time.Second in production; tests use a short interval
	// to stay fast. Zero disables retries (not recommended outside tests
	// that don't need them).
	StartupRetryInterval time.Duration

	// HealthCheckInterval: how often, once past startup, the hub probes the
	// desk (healthProbeAddress) and checks how long it's been since any
	// desk traffic arrived. Degraded is declared after degradedAfterMisses
	// consecutive intervals with no desk traffic at all. Pass a real-world
	// value like 3*time.Second in production; tests use a short interval to
	// stay fast.
	HealthCheckInterval time.Duration

	// InitialLayout/InitialAuxes seed the admin overlay from
	// config/state.json at startup, so the very first buildConfig() call
	// already reflects persisted admin edits — not just messages arriving
	// after the first AdminEvent.
	InitialLayout []config.LayoutItem
	InitialAuxes  []config.Aux

	// ExternalDevices seeds the relay list from config/global.json.
	ExternalDevices []ExternalDevice

	// AutoHideChannelPatterns/AutoHideGroupPatterns/AutoHideAuxPatterns
	// seed the default-hide name patterns from config/global.json — see
	// config.Global's fields of the same name.
	AutoHideChannelPatterns []string
	AutoHideGroupPatterns   []string
	AutoHideAuxPatterns     []string

	// Debug enables per-hop tracing (every inbound message's source, the
	// fromDesk/loopback relay decisions, and each relay attempt's outcome)
	// — mirrors config/global.json's "debug" field (see admin's Network
	// section). Off by default: at normal desk traffic volume this is
	// noisy, so it's opt-in for diagnosing a specific relay problem.
	Debug bool
}

// New builds a Hub. pipe may be nil (equivalent to an empty pipeline).
func New(d DeskSender, pipe *pipeline.Pipeline, logger *log.Logger, opts Options) *Hub {
	healthCheckInterval := opts.HealthCheckInterval
	if healthCheckInterval <= 0 {
		// time.NewTicker panics on a non-positive interval, and most
		// existing callers (tests especially) have no reason to care about
		// desk-timeout detection — defaulting to "effectively never" keeps
		// HealthCheckInterval opt-in rather than forcing every Options{}
		// construction in the codebase to know about it.
		healthCheckInterval = 24 * time.Hour
	}
	return &Hub{
		desk:                    d,
		pipeline:                pipe,
		log:                     logger,
		deskIP:                  opts.DeskIP,
		deskPort:                opts.DeskPort,
		oscPort:                 opts.OSCPort,
		cache:                   make(map[string]oscmsg.Message),
		status:                  StatusStarting,
		ready:                   make(chan Snapshot, 1),
		clients:                 make(map[string]chan<- oscmsg.Envelope),
		dropStreak:              make(map[string]int),
		lastSendErrLogged:       make(map[string]time.Time),
		lastClientDriven:        make(map[string]time.Time),
		layout:                  opts.InitialLayout,
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

// Ready receives exactly once, the moment the startup sequence first
// completes (status transitions to Healthy). Buffered so the send never
// blocks Run even if nothing is listening.
func (h *Hub) Ready() <-chan Snapshot { return h.ready }

// Run drives the hub: kicks off the startup sequence, then processes one
// inbound event — a desk message, a WS client lifecycle event/message, or
// an admin config change — at a time until ctx is done or all channels are
// closed. This is the single serialization point for all hub state — see
// the plan's "Concurrency model" for why a lone goroutine owns this instead
// of mutex-guarded maps. clientEvents and adminEvents may be nil (a nil
// channel never fires in select, so this degrades cleanly for tests/
// earlier-phase callers that don't need them).
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
			// Nudges past a lost UDP packet — see startupRetryInterval's
			// doc comment. A no-op once healthy.
			if h.status == StatusStarting || h.status == StatusReloading {
				h.advanceStartup()
			}
		case <-health.C:
			h.checkDeskHealth()
		}
	}
}

// checkDeskHealth is health's only responsibility: probe the desk so a
// genuinely idle-but-alive desk still produces traffic to judge liveness by
// (see healthProbeAddress), then declare Degraded if nothing at all —
// probe reply or unprompted flood alike — has arrived from the desk in
// degradedAfterMisses intervals. Only meaningful once past startup: Starting
// and Reloading already have their own stall detection via queryOnce/retry,
// and a startup that's legitimately still walking the parameter list is not
// "the desk went silent." Recovery back to Healthy is detected reactively
// in handleDeskMessage the instant desk traffic actually arrives, not here
// — no need to wait for the next tick once we already know.
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

// pushStatus sends the current status to every connected client — used
// whenever status changes but the config itself hasn't (going Degraded:
// connectivity changed, not topology).
func (h *Hub) pushStatus() {
	status := oscmsg.NewStatusEnvelope(string(h.status), h.locked)
	for id, out := range h.clients {
		h.sendTo(id, out, status)
	}
}

// pushStatusAndConfig sends both — used whenever a client may be holding a
// stale config snapshot from before this transition (recovering from either
// Starting or Degraded).
func (h *Hub) pushStatusAndConfig() {
	status := oscmsg.NewStatusEnvelope(string(h.status), h.locked)
	cfg := oscmsg.NewConfigEnvelope(h.buildConfig())
	for id, out := range h.clients {
		h.sendTo(id, out, status)
		h.sendTo(id, out, cfg)
	}
}

func (h *Hub) handleDeskMessage(msg oscmsg.Message, fromIP string) {
	// Every packet landing on the shared socket passes through here first —
	// from the real desk AND from any external device (e.g. an iPad) that
	// sends to this same port. fromIP is what broadcast()'s fromDesk check
	// keys off, so this is the single most useful trace point for "is the
	// packet even arriving, and from what address."
	// Guarded at the call site — see broadcast's doc comment (client.go):
	// an unguarded variadic debugf call pays argument-boxing allocation
	// cost on every message even when Debug is off, confirmed via
	// allocation profiling on this exact hot path.
	if h.debug {
		h.debugf("desk-path %s from %s", msg.Address, fromIP)
	}

	// Default-deny: only the desk itself and explicitly registered external
	// devices may inject messages on the shared socket. Anything else —
	// a stray device on the network, a port scanner, a spoofed sender —
	// is silently dropped rather than processed and relayed as if it were
	// trusted traffic. A registered device additionally needs Broadcast set
	// to be let in at all; the reverse (registered, Broadcast: false) mirrors
	// the existing egress gate in broadcast() (client.go).
	if fromIP != h.deskIP {
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

	if fromIP == h.deskIP {
		h.lastDeskSeen = time.Now()
		if h.status == StatusDegraded {
			h.status = StatusHealthy
			h.log.Printf("desk health: recovered")
			h.pushStatusAndConfig()
		}
	}

	// Every message is relayed 1:1 regardless of whether its value matches
	// what's already cached — an unconditional value-dedup here used to
	// also gate the relay to external devices, which broke it: an external
	// device (e.g. an iPad, relayed through webmixer as if it were the
	// desk — see hub's package doc) queries independently of our cache, so
	// a reply that happens to match a value we already cached from our own
	// earlier query was silently swallowed instead of relayed, and the
	// device's own query went unanswered. Root-caused live: the iPad's own
	// connect handshake stalled on exactly the three addresses
	// (/Console/Channels/?, /Console/Aux_Outputs/modes/?,
	// /Console/Group_Outputs/modes/?) that webmixer's own startup sequence
	// had already cached before the iPad ever asked.
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
		return // matches JS: no broadcast while the startup sequence is still running
	}

	h.broadcast(msg, deskSource(fromIP))
}

// handleSessionChange resets hub to the same zero state as a fresh boot and
// re-enters the startup sequence. This reset is load-bearing, not optional:
// advanceStartup's "already cached, skip" check means a stale cache doesn't
// just fail to help here, it silently defeats the whole re-query — an old
// channel count/name/mode sitting in cache from before the session change
// looks identical to a freshly confirmed one. Confirmed live: sessions can
// differ in channel/aux/group counts, names, and stereo mode.
func (h *Hub) handleSessionChange() {
	h.log.Printf("session change: reloading")

	h.cache = make(map[string]oscmsg.Message)
	h.channelCount, h.auxCount, h.groupCount = 0, 0, 0
	h.snapshotRequested = false
	h.snapshotName = ""
	h.pendingQuery = ""
	h.status = StatusReloading
	h.pushStatus()
	// advanceStartup() is not called here — handleDeskMessage's own
	// status-gated call right after this returns already does it, once.
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

// advanceStartup is the Go port of the JS version's
// loadNextRequiredParameter: called after every inbound message while
// starting/reloading, it sends the next unmet requirement's query and
// returns, or — once everything required is cached — transitions to
// Healthy. Extended per the plan to walk group buses in parallel with
// auxes, not bolted on after the fact.
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

	// Channel stereo mode is a separate address from the aux/group modes
	// arrays above (those cover aux/group buses only) — confirmed live that
	// the real iPad app re-queries this per channel on a session change, and
	// that it can differ session to session. Walked after names, same
	// one-at-a-time discipline as every other step here.
	for i := 1; i <= h.channelCount; i++ {
		key := fmt.Sprintf("/Input_Channels/%d/Channel_Input/stereo_mode", i)
		if _, ok := h.cache[key]; !ok {
			h.queryOnce(key, key+"/?")
			return
		}
	}

	// Snapshot is fire-and-forget, same as the JS version: requested once,
	// but its reply does not gate the transition to Healthy below.
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

		// A client connected while Starting or Reloading only got a
		// status+config snapshot of that in-progress state at connect time
		// (handleClientEvent, handleSessionChange) — nothing else
		// automatically re-pushes it once healthy, since a plain desk
		// message broadcast is just that one address's value, not a status
		// or config refresh. Applies equally to the boot case (a client
		// that connected mid-startup) and the reload case, so it's
		// unconditional here rather than gated on which state we came from.
		h.pushStatusAndConfig()
	}
}

func (h *Hub) query(address string) {
	if err := h.desk.Send(oscmsg.Message{Address: address, Args: []any{}}); err != nil {
		h.log.Printf("query %s: %v", address, err)
	}
}

// queryOnce sends address unless a query for the same cacheKey is already
// outstanding and hasn't had time to plausibly be lost yet.
//
// Without this guard, advanceStartup being re-invoked reactively on every
// inbound message — not just the one it's actually waiting on — means any
// backlog of unrelated desk traffic arriving while one query is in flight
// re-sends that same query once per backlogged message, and the desk
// dutifully answers every one. Confirmed live: a single reply's count grew
// from 1 (nothing queued yet, right at boot) to 14 (deep into the startup
// walk, after a large backlog had built up) — previously invisible because
// the old value-dedup silently absorbed every redundant reply after the
// first (see handleDeskMessage's doc comment for why that dedup had to go;
// this is the gap its removal exposed, not one it caused).
//
// startupRetryInterval doubles as the "how long is too long to wait"
// threshold here — the same interval Run's retry ticker already uses, so a
// genuine retry (packet actually lost) still gets through once that long
// has passed, while a same-microsecond re-entrant call does not.
func (h *Hub) queryOnce(cacheKey, address string) {
	if h.pendingQuery == cacheKey && time.Since(h.pendingQuerySince) < h.startupRetryInterval {
		return
	}
	h.pendingQuery = cacheKey
	h.pendingQuerySince = time.Now()
	h.query(address)
}

// intArg reads Args[idx] as an int, accepting either the int32 or float32
// typetag the desk might reply with for a given address (see plan: the
// int32-vs-float32 convention was validated per-address against the real
// desk in Phase 0, not assumed).
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
