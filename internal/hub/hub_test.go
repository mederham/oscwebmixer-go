package hub

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mederham/oscwebmixer-go/internal/config"
	"github.com/mederham/oscwebmixer-go/internal/desk"
	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
	"github.com/mederham/oscwebmixer-go/internal/pipeline"
)

// scriptedDesk is a minimal fake DiGiCo desk for testing the startup
// sequencer without real hardware. Reply shapes (int32 counts/modes,
// float32 continuous values, a name reply carrying zero args when unset)
// mirror what was confirmed live against the real desk in Phase 0.
type scriptedDesk struct {
	inbound      chan desk.InboundMessage
	sent         chan oscmsg.Message // records every message passed to Send (nil if the test doesn't need it)
	sentExternal chan externalSend   // records every message passed to SendTo (nil if the test doesn't need it)

	channelCount, auxCount, groupCount int
	channelNames, auxNames, groupNames map[int]string // missing key = unnamed
	channelStereo                      map[int]bool   // missing key = mono

	mu       sync.Mutex
	dropOnce map[string]bool // query address -> drop the next reply to it once, simulating lost UDP

	// remoteCalls/rebindCalls record every SetRemote/Rebind call, for
	// TestAdminNetworkApply* to assert against — both the values passed
	// and, just as importantly, how many times each was called (proving
	// an unchanged field doesn't trigger a redundant rebind/address
	// change). setRemoteErr/rebindErr let a test force a failure.
	remoteCalls  []remoteCall
	rebindCalls  []int
	setRemoteErr error
	rebindErr    error
}

type remoteCall struct {
	ip   string
	port int
}

// simulateDroppedReply marks address so its next query is answered with
// silence instead of a reply — exactly once — to test the startup retry
// ticker's recovery from a lost UDP packet (confirmed live against the
// real desk: see startupRetryInterval's doc comment in hub.go).
func (d *scriptedDesk) simulateDroppedReply(address string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dropOnce == nil {
		d.dropOnce = make(map[string]bool)
	}
	d.dropOnce[address] = true
}

// consumeDrop reports whether address's next reply should be dropped,
// clearing the flag so only the first attempt is affected.
func (d *scriptedDesk) consumeDrop(address string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dropOnce[address] {
		delete(d.dropOnce, address)
		return true
	}
	return false
}

var (
	reAuxName       = regexp.MustCompile(`^/Aux_Outputs/(\d+)/Buss_Trim/name/\?$`)
	reGroupName     = regexp.MustCompile(`^/Group_Outputs/(\d+)/Buss_Trim/name/\?$`)
	reChannelName   = regexp.MustCompile(`^/Input_Channels/(\d+)/Channel_Input/name/\?$`)
	reChannelStereo = regexp.MustCompile(`^/Input_Channels/(\d+)/Channel_Input/stereo_mode/\?$`)
)

func (d *scriptedDesk) Send(msg oscmsg.Message) error {
	if d.sent != nil {
		select {
		case d.sent <- msg:
		default:
		}
	}
	if d.consumeDrop(msg.Address) {
		return nil // simulated lost packet: no reply this time
	}
	switch {
	case msg.Address == "/Console/Channels/?":
		d.reply("/Console/Input_Channels", int32(d.channelCount))
	case msg.Address == "/Console/Aux_Outputs/modes/?":
		d.reply("/Console/Aux_Outputs/modes", intArgs(d.auxCount, 1)...)
	case msg.Address == "/Console/Group_Outputs/modes/?":
		d.reply("/Console/Group_Outputs/modes", intArgs(d.groupCount, 2)...)
	case reAuxName.MatchString(msg.Address):
		i := indexArg(reAuxName, msg.Address)
		d.replyName(fmt.Sprintf("/Aux_Outputs/%d/Buss_Trim/name", i), d.auxNames[i])
	case reGroupName.MatchString(msg.Address):
		i := indexArg(reGroupName, msg.Address)
		d.replyName(fmt.Sprintf("/Group_Outputs/%d/Buss_Trim/name", i), d.groupNames[i])
	case reChannelName.MatchString(msg.Address):
		i := indexArg(reChannelName, msg.Address)
		d.replyName(fmt.Sprintf("/Input_Channels/%d/Channel_Input/name", i), d.channelNames[i])
	case reChannelStereo.MatchString(msg.Address):
		i := indexArg(reChannelStereo, msg.Address)
		mode := int32(1)
		if d.channelStereo[i] {
			mode = 2
		}
		d.reply(fmt.Sprintf("/Input_Channels/%d/Channel_Input/stereo_mode", i), mode)
	case msg.Address == "/Snapshots/Current_Snapshot/?":
		d.reply("/Snapshots/Current_Snapshot", int32(-1))
	}
	return nil
}

// externalSend records one SendTo call for test assertions.
type externalSend struct {
	Message oscmsg.Message
	Addr    *net.UDPAddr
}

// SendTo satisfies DeskSender's external-device relay method, recording
// each call onto d.sentExternal (nil if the test doesn't need it).
func (d *scriptedDesk) SendTo(msg oscmsg.Message, addr *net.UDPAddr) error {
	if d.sentExternal != nil {
		select {
		case d.sentExternal <- externalSend{Message: msg, Addr: addr}:
		default:
		}
	}
	return nil
}

func (d *scriptedDesk) SetRemote(ip string, port int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.remoteCalls = append(d.remoteCalls, remoteCall{ip: ip, port: port})
	return d.setRemoteErr
}

func (d *scriptedDesk) Rebind(port int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rebindCalls = append(d.rebindCalls, port)
	return d.rebindErr
}

func (d *scriptedDesk) remoteCallsSnapshot() []remoteCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]remoteCall(nil), d.remoteCalls...)
}

func (d *scriptedDesk) rebindCallsSnapshot() []int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]int(nil), d.rebindCalls...)
}

// replyName sends a populated name reply, or a zero-arg reply when name is
// empty — matching the real desk's confirmed behavior for an unset label.
func (d *scriptedDesk) replyName(address, name string) {
	if name == "" {
		d.reply(address)
		return
	}
	d.reply(address, name)
}

func (d *scriptedDesk) reply(address string, args ...any) {
	if args == nil {
		args = []any{}
	}
	d.inbound <- desk.InboundMessage{Message: oscmsg.Message{Address: address, Args: args}}
}

func intArgs(n, mode int) []any {
	args := make([]any, n)
	for i := range args {
		args[i] = int32(mode)
	}
	return args
}

func indexArg(re *regexp.Regexp, address string) int {
	i, _ := strconv.Atoi(re.FindStringSubmatch(address)[1])
	return i
}

func TestStartupSequenceWithGroups(t *testing.T) {
	fakeDesk := &scriptedDesk{
		inbound:      make(chan desk.InboundMessage, 64),
		channelCount: 2,
		auxCount:     2,
		groupCount:   2,
		channelNames: map[int]string{1: "Kick", 2: "Snare"},
		auxNames:     map[int]string{1: "Mon 1", 2: "Mon 2"},
		groupNames:   map[int]string{2: "Drums"}, // group 1 intentionally left unnamed
	}

	h := New(fakeDesk, nil, log.New(io.Discard, "", 0), Options{StartupRetryInterval: 50 * time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	runErr := make(chan error, 1)
	go func() { runErr <- h.Run(ctx, fakeDesk.inbound, nil, nil) }()

	var snap Snapshot
	select {
	case snap = <-h.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("startup did not complete within timeout")
	}

	if snap.Channels != 2 || snap.Auxes != 2 || snap.Groups != 2 {
		t.Fatalf("counts = %+v, want {2 2 2}", snap)
	}

	cancel()
	select {
	case err := <-runErr:
		if err != context.Canceled {
			t.Fatalf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

func TestUnnamedBusCachesAsZeroArgs(t *testing.T) {
	fakeDesk := &scriptedDesk{
		inbound:      make(chan desk.InboundMessage, 64),
		channelCount: 1,
		auxCount:     1,
		groupCount:   1,
		channelNames: map[int]string{1: "Kick"},
		auxNames:     map[int]string{1: "Mon 1"},
		groupNames:   map[int]string{}, // group 1 unnamed
	}

	h := New(fakeDesk, nil, log.New(io.Discard, "", 0), Options{StartupRetryInterval: 50 * time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go h.Run(ctx, fakeDesk.inbound, nil, nil)

	select {
	case <-h.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("startup did not complete within timeout")
	}

	// Give the healthy-triggering message's own cache write a moment to
	// land before we assert on it — Ready() fires from inside the same
	// synchronous call that already wrote the cache, so this is really
	// just documenting that no extra wait is needed, not racing the hub.
	cached, ok := h.cache["/Group_Outputs/1/Buss_Trim/name"]
	if !ok {
		t.Fatal("unnamed group's name address was not cached at all")
	}
	if len(cached.Args) != 0 {
		t.Fatalf("args = %v, want empty (unnamed bus)", cached.Args)
	}
}

// countingInterceptor counts how many messages actually reach the
// pipeline. Uses atomic.Int64 since Handle runs on the hub goroutine while
// the test reads the count from its own goroutine.
type countingInterceptor struct{ n atomic.Int64 }

func (c *countingInterceptor) Handle(msg oscmsg.Message) (oscmsg.Message, bool) {
	c.n.Add(1)
	return msg, true
}

// waitForQuiet polls counter until it stops changing for quietFor, then
// returns its value — used instead of reading counter.n right after
// Ready() fires, because the fire-and-forget snapshot query's reply
// (advanceStartup's last step) is sent before Ready() but not necessarily
// processed until after: Ready() fires synchronously mid-call, while the
// reply it triggered still has to go all the way back around through
// Run()'s select loop. Reading the counter immediately after Ready()
// sometimes wins that race and sometimes loses it — this waits it out
// instead of guessing with a fixed sleep.
func waitForQuiet(counter *countingInterceptor, timeout, quietFor time.Duration) int64 {
	deadline := time.Now().Add(timeout)
	last := counter.n.Load()
	stableSince := time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		cur := counter.n.Load()
		if cur != last {
			last = cur
			stableSince = time.Now()
			continue
		}
		if time.Since(stableSince) >= quietFor {
			return last
		}
	}
	return last
}

// TestDuplicateResendIsStillRelayed is the regression test for a real bug
// found by debug-tracing a live iPad-through-webmixer-to-desk session: an
// exact repeat of an already-cached value was being dropped before it ever
// reached the pipeline or broadcast() — which silently ate replies to an
// external device's own query whenever that value happened to already be
// cached from webmixer's own earlier query of the same address (see
// handleDeskMessage's doc comment). A value-dedup must never gate relay —
// every message reaches the pipeline and gets broadcast, whether its value
// is new or an exact repeat.
func TestDuplicateResendIsStillRelayed(t *testing.T) {
	fakeDesk := &scriptedDesk{
		inbound:      make(chan desk.InboundMessage, 64),
		channelCount: 1,
		auxCount:     1,
		groupCount:   1,
		channelNames: map[int]string{1: "Kick"},
		auxNames:     map[int]string{1: "Mon 1"},
		groupNames:   map[int]string{1: "Drums"},
	}

	counter := &countingInterceptor{}
	h := New(fakeDesk, pipeline.New(counter), log.New(io.Discard, "", 0), Options{StartupRetryInterval: 50 * time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go h.Run(ctx, fakeDesk.inbound, nil, nil)

	select {
	case <-h.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("startup did not complete within timeout")
	}
	// The fire-and-forget snapshot query's reply can land just after
	// Ready() fires (see waitForQuiet's doc comment) — settle first so it
	// doesn't get miscounted as one of this test's own two messages below.
	afterStartup := waitForQuiet(counter, 2*time.Second, 20*time.Millisecond)

	// Resend the exact same channel-1-name value the desk already sent
	// during startup — must still reach the pipeline (and be relayed).
	fakeDesk.inbound <- desk.InboundMessage{
		Message: oscmsg.Message{Address: "/Input_Channels/1/Channel_Input/name", Args: []any{"Kick"}},
	}
	// A genuinely new value on the same address must reach the pipeline too.
	fakeDesk.inbound <- desk.InboundMessage{
		Message: oscmsg.Message{Address: "/Input_Channels/1/Channel_Input/name", Args: []any{"Renamed"}},
	}

	// Drain deterministically: push a distinguishable sentinel through the
	// same serialized queue and wait for it, rather than sleeping.
	sentinelDone := make(chan struct{})
	go func() {
		for counter.n.Load() < afterStartup+2 {
			time.Sleep(time.Millisecond)
		}
		close(sentinelDone)
	}()
	select {
	case <-sentinelDone:
	case <-time.After(2 * time.Second):
		t.Fatal("not both the duplicate and the genuinely new value reached the pipeline")
	}

	if got := counter.n.Load() - afterStartup; got != 2 {
		t.Fatalf("pipeline invoked %d time(s) after startup for one duplicate + one real change, want 2", got)
	}
}

// drainN reads exactly n envelopes off ch, failing the test if any read
// times out — used to consume a client's known initial status+config pair
// before making assertions about what arrives afterward.
func drainN(t *testing.T, ch <-chan oscmsg.Envelope, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("expected %d envelope(s), timed out after %d", n, i)
		}
	}
}

// drainQuiet discards envelopes on ch until it goes quiet for quietWindow —
// for sweeping up a still-in-flight straggler after drainN's exact count
// (the fire-and-forget snapshot query's reply, broadcast to clients as a
// plain OSC envelope once status is Healthy, can still be in transit right
// after a client connects — same race as drainAllSent/drainAllSentExternal,
// here on the per-client envelope channel instead of the desk-facing ones).
func drainQuiet(ch <-chan oscmsg.Envelope) {
	quietSince := time.Now()
	for time.Since(quietSince) < quietWindow {
		select {
		case <-ch:
			quietSince = time.Now()
		case <-time.After(quietPollInterval):
		}
	}
}

// quietPollInterval/quietWindow tune drainAllSent/drainAllSentExternal's
// wait for the fire-and-forget snapshot query's reply — it can still be in
// flight (not yet on the channel at all) right when Ready() fires, so a
// single non-blocking drain pass isn't enough to catch it; this waits for
// the channel to go quiet instead of assuming whatever's already sitting
// there is everything.
const (
	quietPollInterval = time.Millisecond
	quietWindow       = 20 * time.Millisecond
)

// drainAllSent empties d.sent of whatever accumulated during startup,
// waiting for a quiet period so a still-in-flight straggler is caught too —
// so a later "desk received nothing new" assertion has a clean baseline.
func drainAllSent(d *scriptedDesk) {
	quietSince := time.Now()
	for time.Since(quietSince) < quietWindow {
		select {
		case <-d.sent:
			quietSince = time.Now()
		case <-time.After(quietPollInterval):
		}
	}
}

// drainAllSentExternal is drainAllSent's counterpart for d.sentExternal —
// needed because the fire-and-forget snapshot query's reply arrives (and
// gets broadcast to external devices) asynchronously, sometimes after
// Ready() fires, so it can otherwise still be sitting in the channel when
// a later test starts asserting on it.
func drainAllSentExternal(d *scriptedDesk) {
	quietSince := time.Now()
	for time.Since(quietSince) < quietWindow {
		select {
		case <-d.sentExternal:
			quietSince = time.Now()
		case <-time.After(quietPollInterval):
		}
	}
}

func newTestHubWithOneOfEach(t *testing.T) (*Hub, *scriptedDesk, chan ClientEvent) {
	t.Helper()
	fakeDesk := &scriptedDesk{
		inbound:      make(chan desk.InboundMessage, 64),
		sent:         make(chan oscmsg.Message, 64),
		channelCount: 1,
		auxCount:     1,
		groupCount:   1,
		channelNames: map[int]string{1: "Kick"},
		auxNames:     map[int]string{1: "Mon 1"},
		groupNames:   map[int]string{1: "Drums"},
	}
	h := New(fakeDesk, nil, log.New(io.Discard, "", 0), Options{StartupRetryInterval: 50 * time.Millisecond})
	clientEvents := make(chan ClientEvent, 8)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	go h.Run(ctx, fakeDesk.inbound, clientEvents, nil)

	select {
	case <-h.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("startup did not complete")
	}
	drainAllSent(fakeDesk)

	return h, fakeDesk, clientEvents
}

// TestStartupRecoversFromLostReply proves the retry ticker actually
// recovers startup after a simulated dropped UDP packet — this is a
// regression test for a real bug found live: the real desk startup
// stalled permanently, twice, on ordinary packet loss before this ticker
// existed (advanceStartup only re-ran reactively on new messages, so a
// lost reply with no other traffic arriving meant it just hung forever).
func TestStartupRecoversFromLostReply(t *testing.T) {
	fakeDesk := &scriptedDesk{
		inbound:      make(chan desk.InboundMessage, 64),
		channelCount: 2,
		auxCount:     1,
		groupCount:   1,
		channelNames: map[int]string{1: "Kick", 2: "Snare"},
		auxNames:     map[int]string{1: "Mon 1"},
		groupNames:   map[int]string{1: "Drums"},
	}
	// Simulate the exact failure mode observed live: the reply to a
	// mid-sequence query (channel 2's name) never arrives the first time.
	fakeDesk.simulateDroppedReply("/Input_Channels/2/Channel_Input/name/?")

	h := New(fakeDesk, nil, log.New(io.Discard, "", 0), Options{StartupRetryInterval: 30 * time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go h.Run(ctx, fakeDesk.inbound, nil, nil)

	select {
	case <-h.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("startup never recovered from the simulated dropped reply — the retry ticker isn't working")
	}
}

// TestAdvanceStartupDoesNotResendPendingQuery is the regression test for a
// real bug found live: advanceStartup is re-invoked reactively on every
// inbound message, not just the one it's actually waiting on — with no
// guard against re-sending an already-outstanding query, a backlog of
// unrelated desk traffic arriving while one query was in flight caused a
// resend per backlogged message, and the desk dutifully answered every one
// (confirmed live: a single reply's count grew from 1, sent right at boot
// before any backlog existed, to 14 deep into the startup walk after a
// large backlog had built up). This was silent before — the old
// value-dedup absorbed every redundant reply after the first — and only
// became visible once removing that dedup (see handleDeskMessage's doc
// comment) let every one of those replies through. queryOnce is the actual
// fix; this proves the guard holds under exactly that backlog condition.
func TestAdvanceStartupDoesNotResendPendingQuery(t *testing.T) {
	fakeDesk := &scriptedDesk{
		inbound:      make(chan desk.InboundMessage, 64),
		sent:         make(chan oscmsg.Message, 64),
		channelCount: 1, auxCount: 1, groupCount: 1,
		channelNames: map[int]string{1: "Kick"},
		auxNames:     map[int]string{1: "Mon 1"},
		groupNames:   map[int]string{1: "Drums"},
	}
	// Drop the very first reply so /Console/Channels/? stays genuinely
	// pending — a long retry interval keeps the ticker itself from sending
	// a legitimate second attempt during this test's short window.
	fakeDesk.simulateDroppedReply("/Console/Channels/?")

	h := New(fakeDesk, nil, log.New(io.Discard, "", 0), Options{StartupRetryInterval: 5 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go h.Run(ctx, fakeDesk.inbound, nil, nil)

	select {
	case sent := <-fakeDesk.sent:
		if sent.Address != "/Console/Channels/?" {
			t.Fatalf("first query = %+v, want /Console/Channels/?", sent)
		}
	case <-time.After(time.Second):
		t.Fatal("initial query was never sent")
	}

	// Flood in unrelated backlog traffic while the reply is still pending —
	// each one re-invokes advanceStartup via handleDeskMessage's status
	// gate, same as real desk chatter arriving during a real startup walk.
	for i := 0; i < 20; i++ {
		fakeDesk.inbound <- desk.InboundMessage{
			Message: oscmsg.Message{Address: "/Meters/clear", Args: []any{}},
		}
	}

	select {
	case sent := <-fakeDesk.sent:
		t.Fatalf("unrelated backlog traffic caused a resend: %+v — queryOnce's guard did not hold", sent)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestClientConnectReceivesStatusAndConfig(t *testing.T) {
	_, _, clientEvents := newTestHubWithOneOfEach(t)

	outbound := make(chan oscmsg.Envelope, 8)
	clientEvents <- ClientEvent{Kind: ClientConnected, ClientID: "c1", Outbound: outbound}

	var statusEnv, configEnv oscmsg.Envelope
	select {
	case statusEnv = <-outbound:
	case <-time.After(time.Second):
		t.Fatal("did not receive status envelope")
	}
	select {
	case configEnv = <-outbound:
	case <-time.After(time.Second):
		t.Fatal("did not receive config envelope")
	}

	if statusEnv.Kind != oscmsg.KindStatus || statusEnv.Status == nil || statusEnv.Status.Desk != string(StatusHealthy) {
		t.Fatalf("status envelope = %+v", statusEnv)
	}
	if configEnv.Kind != oscmsg.KindConfig || configEnv.Config == nil {
		t.Fatalf("config envelope = %+v", configEnv)
	}
	if len(configEnv.Config.MixerLayout) != 2 {
		t.Fatalf("mixer layout = %+v, want 2 (1 channel + 1 group)", configEnv.Config.MixerLayout)
	}
	if configEnv.Config.MixerLayout[0].Type != "channel" || configEnv.Config.MixerLayout[1].Type != "group" {
		t.Fatalf("layout order = %+v, want channel then group (natural desk order fallback)", configEnv.Config.MixerLayout)
	}
	if len(configEnv.Config.Auxes) != 1 || configEnv.Config.Auxes[0].Label != "Mon 1" {
		t.Fatalf("auxes = %+v", configEnv.Config.Auxes)
	}
}

func TestClientMessageBroadcastsToOthersNotSender(t *testing.T) {
	_, fakeDesk, clientEvents := newTestHubWithOneOfEach(t)

	out1 := make(chan oscmsg.Envelope, 8)
	out2 := make(chan oscmsg.Envelope, 8)
	clientEvents <- ClientEvent{Kind: ClientConnected, ClientID: "c1", Outbound: out1}
	clientEvents <- ClientEvent{Kind: ClientConnected, ClientID: "c2", Outbound: out2}
	drainN(t, out1, 2)
	drainN(t, out2, 2)

	moveMsg := oscmsg.Message{Address: "/Input_Channels/1/Aux_Send/1/send_level", Args: []any{float32(-6)}}
	clientEvents <- ClientEvent{Kind: ClientMessage, ClientID: "c1", Message: moveMsg}

	select {
	case env := <-out2:
		if env.Kind != oscmsg.KindOSC || env.Message == nil || env.Message.Address != moveMsg.Address {
			t.Fatalf("c2 received %+v, want the fader move", env)
		}
	case <-time.After(time.Second):
		t.Fatal("c2 did not receive the broadcast fader move")
	}

	select {
	case env := <-out1:
		t.Fatalf("sender received an echo of its own message: %+v", env)
	case <-time.After(200 * time.Millisecond):
	}

	select {
	case sent := <-fakeDesk.sent:
		if sent.Address != moveMsg.Address {
			t.Fatalf("desk received %+v, want the fader move", sent)
		}
	case <-time.After(time.Second):
		t.Fatal("desk did not receive the fader move")
	}
}

func TestClientQueryRespondsFromCacheOnly(t *testing.T) {
	_, fakeDesk, clientEvents := newTestHubWithOneOfEach(t)

	out1 := make(chan oscmsg.Envelope, 8)
	out2 := make(chan oscmsg.Envelope, 8)
	clientEvents <- ClientEvent{Kind: ClientConnected, ClientID: "c1", Outbound: out1}
	clientEvents <- ClientEvent{Kind: ClientConnected, ClientID: "c2", Outbound: out2}
	drainN(t, out1, 2)
	drainN(t, out2, 2)

	queryMsg := oscmsg.Message{Address: "/Input_Channels/1/Channel_Input/name/?", Args: []any{}}
	clientEvents <- ClientEvent{Kind: ClientMessage, ClientID: "c1", Message: queryMsg}

	select {
	case env := <-out1:
		if env.Kind != oscmsg.KindOSC || env.Message == nil || env.Message.Address != "/Input_Channels/1/Channel_Input/name" {
			t.Fatalf("c1 received %+v, want the cached channel-name reply", env)
		}
	case <-time.After(time.Second):
		t.Fatal("c1 did not receive a reply to its own query")
	}

	select {
	case env := <-out2:
		t.Fatalf("c2 received something it shouldn't have (a cache-answered query must not broadcast): %+v", env)
	case <-time.After(200 * time.Millisecond):
	}

	select {
	case sent := <-fakeDesk.sent:
		t.Fatalf("desk received the query %+v, want it answered from cache without forwarding", sent)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestExternalDeviceMessagesAreForwardedToDesk is the regression test for a
// real bug found by asking "are the iPad's OSC messages routed to the
// console and vice versa" against the live desk: a message arriving on the
// shared OSC socket from a registered external device (a different IP than
// the desk) was being unconditionally treated as "from the desk" and
// therefore never forwarded to the actual console. See broadcastSource's
// doc comment and broadcast() in client.go.
func TestExternalDeviceMessagesAreForwardedToDesk(t *testing.T) {
	const deskIP = "10.0.0.18"
	const ipadIP = "10.0.0.12"

	fakeDesk := &scriptedDesk{
		inbound:      make(chan desk.InboundMessage, 64),
		sent:         make(chan oscmsg.Message, 64),
		channelCount: 1, auxCount: 1, groupCount: 1,
		channelNames: map[int]string{1: "Kick"},
		auxNames:     map[int]string{1: "Mon 1"},
		groupNames:   map[int]string{1: "Drums"},
	}
	h := New(fakeDesk, nil, log.New(io.Discard, "", 0), Options{
		DeskIP: deskIP, StartupRetryInterval: 50 * time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go h.Run(ctx, fakeDesk.inbound, nil, nil)

	select {
	case <-h.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("startup did not complete")
	}
	drainAllSent(fakeDesk)

	// A message arriving from the iPad's IP — not the desk's — must still
	// reach the desk.
	fakeDesk.inbound <- desk.InboundMessage{
		Message: oscmsg.Message{Address: "/Console/Name/?", Args: []any{}},
		From:    &net.UDPAddr{IP: net.ParseIP(ipadIP), Port: 9000},
	}

	select {
	case sent := <-fakeDesk.sent:
		if sent.Address != "/Console/Name/?" {
			t.Fatalf("desk received %+v, want the iPad's forwarded message", sent)
		}
	case <-time.After(time.Second):
		t.Fatal("message from a non-desk IP was never forwarded to the desk")
	}
}

// TestDeskRepliesAreNotEchoedBackToDesk confirms the other half stays
// correct: a message whose sender IP matches the configured desk must NOT
// be sent straight back to it.
func TestDeskRepliesAreNotEchoedBackToDesk(t *testing.T) {
	const deskIP = "10.0.0.18"

	fakeDesk := &scriptedDesk{
		inbound:      make(chan desk.InboundMessage, 64),
		sent:         make(chan oscmsg.Message, 64),
		channelCount: 1, auxCount: 1, groupCount: 1,
		channelNames: map[int]string{1: "Kick"},
		auxNames:     map[int]string{1: "Mon 1"},
		groupNames:   map[int]string{1: "Drums"},
	}
	h := New(fakeDesk, nil, log.New(io.Discard, "", 0), Options{
		DeskIP: deskIP, StartupRetryInterval: 50 * time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go h.Run(ctx, fakeDesk.inbound, nil, nil)

	select {
	case <-h.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("startup did not complete")
	}
	drainAllSent(fakeDesk)

	fakeDesk.inbound <- desk.InboundMessage{
		Message: oscmsg.Message{Address: "/Some/Desk/Value", Args: []any{float32(1)}},
		From:    &net.UDPAddr{IP: net.ParseIP(deskIP), Port: 9000},
	}

	select {
	case sent := <-fakeDesk.sent:
		t.Fatalf("desk received an echo of its own message: %+v", sent)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestExternalDeviceLoopbackExclusion confirms an external device doesn't
// receive an echo of its own message back (Loopback: false), while a
// different external device still gets it normally.
func TestExternalDeviceLoopbackExclusion(t *testing.T) {
	const deskIP = "10.0.0.18"
	const ipadIP = "10.0.0.12"
	const otherIP = "10.0.0.50"

	fakeDesk := &scriptedDesk{
		inbound:      make(chan desk.InboundMessage, 64),
		sent:         make(chan oscmsg.Message, 64),
		sentExternal: make(chan externalSend, 64),
		channelCount: 1, auxCount: 1, groupCount: 1,
		channelNames: map[int]string{1: "Kick"},
		auxNames:     map[int]string{1: "Mon 1"},
		groupNames:   map[int]string{1: "Drums"},
	}
	h := New(fakeDesk, nil, log.New(io.Discard, "", 0), Options{
		DeskIP:               deskIP,
		StartupRetryInterval: 50 * time.Millisecond,
		ExternalDevices: []ExternalDevice{
			{Name: "iPad", Addr: &net.UDPAddr{IP: net.ParseIP(ipadIP), Port: 9000}, Broadcast: true, Loopback: false},
			{Name: "Other", Addr: &net.UDPAddr{IP: net.ParseIP(otherIP), Port: 9000}, Broadcast: true, Loopback: false},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go h.Run(ctx, fakeDesk.inbound, nil, nil)

	select {
	case <-h.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("startup did not complete")
	}
	drainAllSent(fakeDesk)
	drainAllSentExternal(fakeDesk)

	fakeDesk.inbound <- desk.InboundMessage{
		Message: oscmsg.Message{Address: "/Console/Name/?", Args: []any{}},
		From:    &net.UDPAddr{IP: net.ParseIP(ipadIP), Port: 9000},
	}

	// Filter by address too, not just drain-before: the fire-and-forget
	// snapshot query's reply can arrive and get broadcast to external
	// devices asynchronously, sometimes racing this assertion — ignoring
	// anything but the message this test actually sent keeps the
	// assertion meaningful regardless of that timing.
	var gotOther bool
	deadline := time.After(time.Second)
	for !gotOther {
		select {
		case sent := <-fakeDesk.sentExternal:
			if sent.Message.Address != "/Console/Name/?" {
				continue
			}
			if sent.Addr.IP.String() == ipadIP {
				t.Fatalf("iPad received an echo of its own message: %+v", sent)
			}
			if sent.Addr.IP.String() == otherIP {
				gotOther = true
			}
		case <-deadline:
			t.Fatal("the other external device never received the relayed message")
		}
	}
}

// TestExternalDeviceGetsReplyEvenWhenValueMatchesCache is the regression
// test for the actual live-session bug (see handleDeskMessage's doc
// comment): an external device relayed through webmixer as if it were the
// desk (e.g. an iPad) sends its own /Console/Channels/? during its connect
// handshake. webmixer had already cached the reply address
// (/Console/Input_Channels) from its own earlier startup query with the
// same value — the old value-dedup swallowed the reply before it ever
// reached broadcast(), so the iPad's query silently went unanswered and its
// own handshake stalled. The reply must always reach the external device,
// whether or not its value is new to webmixer's cache.
func TestExternalDeviceGetsReplyEvenWhenValueMatchesCache(t *testing.T) {
	const deskIP = "10.0.0.18"
	const ipadIP = "10.0.0.12"

	fakeDesk := &scriptedDesk{
		inbound:      make(chan desk.InboundMessage, 64),
		sentExternal: make(chan externalSend, 64),
		channelCount: 1, auxCount: 1, groupCount: 1,
		channelNames: map[int]string{1: "Kick"},
		auxNames:     map[int]string{1: "Mon 1"},
		groupNames:   map[int]string{1: "Drums"},
	}
	h := New(fakeDesk, nil, log.New(io.Discard, "", 0), Options{
		DeskIP:               deskIP,
		StartupRetryInterval: 50 * time.Millisecond,
		ExternalDevices: []ExternalDevice{
			{Name: "iPad", Addr: &net.UDPAddr{IP: net.ParseIP(ipadIP), Port: 9000}, Broadcast: true, Loopback: false},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go h.Run(ctx, fakeDesk.inbound, nil, nil)

	select {
	case <-h.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("startup did not complete")
	}
	drainAllSentExternal(fakeDesk)

	// Re-query the exact address webmixer's own startup already cached —
	// scriptedDesk.Send answers /Console/Channels/? with
	// /Console/Input_Channels carrying the same channelCount value.
	fakeDesk.inbound <- desk.InboundMessage{
		Message: oscmsg.Message{Address: "/Console/Channels/?", Args: []any{}},
		From:    &net.UDPAddr{IP: net.ParseIP(ipadIP), Port: 9000},
	}

	deadline := time.After(time.Second)
	for {
		select {
		case sent := <-fakeDesk.sentExternal:
			if sent.Message.Address == "/Console/Input_Channels" && sent.Addr.IP.String() == ipadIP {
				return // relayed, as required
			}
		case <-deadline:
			t.Fatal("the iPad's query for an already-cached value never got a relayed reply")
		}
	}
}

// TestSessionChangeReQueriesFreshTopology is the regression test for Phase
// 4's session-reload handling: confirmed live (two packet captures) that a
// session change on the real desk never pushes topology (counts, names,
// modes, channel stereo) unprompted — the desk floods ~700 value updates
// but not that, and the real iPad app itself explicitly re-queries it
// fresh. This proves handleSessionChange's cache reset actually forces a
// real re-query rather than advanceStartup seeing stale-but-present cache
// entries and silently no-op'ing through to Healthy — by changing what the
// (fake) desk reports mid-test and confirming the reload picks up the new
// values, not the old ones.
//
// Deliberately observed only through a connected client's own envelope
// stream (status: reloading, then status: healthy + fresh config) rather
// than reading hub's internal fields directly — hub state is only safe to
// read from its own goroutine (see package doc), and this is exactly the
// request/response-shaped channel hub already provides for the purpose.
func TestSessionChangeReQueriesFreshTopology(t *testing.T) {
	fakeDesk := &scriptedDesk{
		inbound:      make(chan desk.InboundMessage, 64),
		channelCount: 1, auxCount: 1, groupCount: 1,
		channelNames: map[int]string{1: "Kick"},
		auxNames:     map[int]string{1: "Mon 1"},
		groupNames:   map[int]string{1: "Drums"},
	}
	h := New(fakeDesk, nil, log.New(io.Discard, "", 0), Options{StartupRetryInterval: 50 * time.Millisecond})
	clientEvents := make(chan ClientEvent, 8)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go h.Run(ctx, fakeDesk.inbound, clientEvents, nil)

	select {
	case <-h.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("startup did not complete")
	}

	out := make(chan oscmsg.Envelope, 16)
	clientEvents <- ClientEvent{Kind: ClientConnected, ClientID: "c1", Outbound: out}
	drainN(t, out, 2) // initial status(healthy) + config, reflecting session 1
	drainQuiet(out)   // sweep up the boot snapshot query's straggler reply, if any

	// Change what the "desk" reports — simulating a different session:
	// more channels, different names, channel 1 now stereo.
	fakeDesk.channelCount = 2
	fakeDesk.channelNames = map[int]string{1: "Vocal L", 2: "Vocal R"}
	fakeDesk.channelStereo = map[int]bool{1: true}
	fakeDesk.auxNames = map[int]string{1: "Renamed Aux"}
	fakeDesk.groupNames = map[int]string{1: "Renamed Group"}

	fakeDesk.inbound <- desk.InboundMessage{
		Message: oscmsg.Message{Address: sessionChangeAddress, Args: []any{int32(0)}},
	}

	var reloadingEnv oscmsg.Envelope
	select {
	case reloadingEnv = <-out:
	case <-time.After(time.Second):
		t.Fatal("did not receive a status envelope after the session change")
	}
	if reloadingEnv.Kind != oscmsg.KindStatus || reloadingEnv.Status == nil || reloadingEnv.Status.Desk != string(StatusReloading) {
		t.Fatalf("first envelope after session change = %+v, want status:reloading", reloadingEnv)
	}

	var healthyEnv, configEnv oscmsg.Envelope
	select {
	case healthyEnv = <-out:
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive a status envelope after the reload completed")
	}
	select {
	case configEnv = <-out:
	case <-time.After(time.Second):
		t.Fatal("did not receive a config envelope after the reload completed")
	}
	if healthyEnv.Kind != oscmsg.KindStatus || healthyEnv.Status == nil || healthyEnv.Status.Desk != string(StatusHealthy) {
		t.Fatalf("post-reload status envelope = %+v, want status:healthy", healthyEnv)
	}
	if configEnv.Kind != oscmsg.KindConfig || configEnv.Config == nil {
		t.Fatalf("post-reload envelope = %+v, want config", configEnv)
	}

	sources := configEnv.Config.MixerLayout
	if len(sources) != 3 { // 2 channels + 1 group
		t.Fatalf("mixer layout = %+v, want 3 (2 channels + 1 group, session 2's counts)", sources)
	}
	byNumber := map[int]oscmsg.LayoutItem{}
	for _, s := range sources {
		if s.Type == "channel" {
			byNumber[s.Number] = s
		}
	}
	if byNumber[1].Label != "Vocal L" || !byNumber[1].Stereo {
		t.Fatalf("channel 1 = %+v, want label %q stereo=true (session 2's data, not stale session 1)", byNumber[1], "Vocal L")
	}
	if byNumber[2].Label != "Vocal R" {
		t.Fatalf("channel 2 = %+v, want label %q — this address didn't exist before the reload, so it can only be here via a real re-query", byNumber[2], "Vocal R")
	}
	if configEnv.Config.Auxes[0].Label != "Renamed Aux" {
		t.Fatalf("aux label = %q, want %q", configEnv.Config.Auxes[0].Label, "Renamed Aux")
	}
}

// TestSessionChangeDropsClientMessages confirms the owner's explicit
// decision: a client-originated change arriving while Reloading is dropped
// outright, not queued or forwarded — a channel/aux number it targets may
// no longer mean the same thing once a different session has loaded.
func TestSessionChangeDropsClientMessages(t *testing.T) {
	fakeDesk := &scriptedDesk{
		inbound:      make(chan desk.InboundMessage, 64),
		sent:         make(chan oscmsg.Message, 64),
		channelCount: 1, auxCount: 1, groupCount: 1,
		channelNames: map[int]string{1: "Kick"},
		auxNames:     map[int]string{1: "Mon 1"},
		groupNames:   map[int]string{1: "Drums"},
	}
	// A long retry interval plus a dropped first reply keeps hub stuck in
	// Reloading for the test's duration — deterministic, not timing-luck.
	h := New(fakeDesk, nil, log.New(io.Discard, "", 0), Options{StartupRetryInterval: 5 * time.Second})
	clientEvents := make(chan ClientEvent, 8)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go h.Run(ctx, fakeDesk.inbound, clientEvents, nil)

	select {
	case <-h.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("startup did not complete")
	}

	outA := make(chan oscmsg.Envelope, 16)
	outB := make(chan oscmsg.Envelope, 16)
	clientEvents <- ClientEvent{Kind: ClientConnected, ClientID: "A", Outbound: outA}
	clientEvents <- ClientEvent{Kind: ClientConnected, ClientID: "B", Outbound: outB}
	drainN(t, outA, 2)
	drainN(t, outB, 2)
	drainQuiet(outA) // sweep up the boot snapshot query's straggler reply, if any
	drainQuiet(outB)
	drainAllSent(fakeDesk)

	fakeDesk.simulateDroppedReply("/Console/Channels/?")
	fakeDesk.inbound <- desk.InboundMessage{
		Message: oscmsg.Message{Address: sessionChangeAddress, Args: []any{int32(0)}},
	}

	// handleSessionChange pushes status:reloading to every connected
	// client, not just the one about to send a message — both A and B get
	// it, so both need draining before checking "nothing further arrives."
	select {
	case env := <-outA:
		if env.Kind != oscmsg.KindStatus || env.Status == nil || env.Status.Desk != string(StatusReloading) {
			t.Fatalf("envelope after session change (A) = %+v, want status:reloading", env)
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive status:reloading on A — reload never started")
	}
	select {
	case env := <-outB:
		if env.Kind != oscmsg.KindStatus || env.Status == nil || env.Status.Desk != string(StatusReloading) {
			t.Fatalf("envelope after session change (B) = %+v, want status:reloading", env)
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive status:reloading on B — reload never started")
	}
	drainAllSent(fakeDesk) // the re-triggered (and dropped) /Console/Channels/? query

	moveMsg := oscmsg.Message{Address: "/Input_Channels/1/Aux_Send/1/send_level", Args: []any{float32(-6)}}
	clientEvents <- ClientEvent{Kind: ClientMessage, ClientID: "A", Message: moveMsg}

	select {
	case env := <-outB:
		t.Fatalf("client B received %+v — a message sent during Reloading must be dropped, not broadcast", env)
	case <-time.After(200 * time.Millisecond):
	}
	select {
	case sent := <-fakeDesk.sent:
		t.Fatalf("desk received %+v — a message sent during Reloading must be dropped, not forwarded", sent)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestDeskTimeoutDeclaresDegradedAndRecovers is the regression test for the
// desk-timeout half of the Healthcheck redesign (plan: "internal/health"):
// checkDeskHealth must actively probe the desk during a quiet period — a
// genuinely idle-but-alive desk (no fader moves, no meter flood) must not be
// misjudged as unresponsive just because nothing unprompted arrived — and
// declare Degraded once nothing at all, probe reply included, has arrived
// for degradedAfterMisses intervals. Recovery must be immediate the instant
// desk traffic actually resumes, not deferred to the next tick, and must
// push a fresh status+config since a client may be holding data from before
// the outage.
func TestDeskTimeoutDeclaresDegradedAndRecovers(t *testing.T) {
	fakeDesk := &scriptedDesk{
		inbound:      make(chan desk.InboundMessage, 64),
		sent:         make(chan oscmsg.Message, 64),
		channelCount: 1, auxCount: 1, groupCount: 1,
		channelNames: map[int]string{1: "Kick"},
		auxNames:     map[int]string{1: "Mon 1"},
		groupNames:   map[int]string{1: "Drums"},
	}
	h := New(fakeDesk, nil, log.New(io.Discard, "", 0), Options{
		StartupRetryInterval: 50 * time.Millisecond,
		HealthCheckInterval:  20 * time.Millisecond,
	})
	clientEvents := make(chan ClientEvent, 8)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go h.Run(ctx, fakeDesk.inbound, clientEvents, nil)

	select {
	case <-h.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("startup did not complete")
	}

	out := make(chan oscmsg.Envelope, 16)
	clientEvents <- ClientEvent{Kind: ClientConnected, ClientID: "c1", Outbound: out}
	drainN(t, out, 2)
	drainQuiet(out)
	drainAllSent(fakeDesk) // clear the startup query traffic

	// Go quiet: nothing sent into fakeDesk.inbound, so scriptedDesk never
	// replies to the health ticker's own probes either — exactly like a
	// real disconnect.
	var degradedEnv oscmsg.Envelope
	select {
	case degradedEnv = <-out:
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive status:degraded after desk silence")
	}
	if degradedEnv.Kind != oscmsg.KindStatus || degradedEnv.Status == nil || degradedEnv.Status.Desk != string(StatusDegraded) {
		t.Fatalf("envelope after silence = %+v, want status:degraded", degradedEnv)
	}

	// Confirm the health ticker was actively probing during the silence,
	// not just passively waiting for unprompted traffic to stop.
	select {
	case sent := <-fakeDesk.sent:
		if sent.Address != healthProbeAddress {
			t.Fatalf("query sent during silence = %q, want %q", sent.Address, healthProbeAddress)
		}
	case <-time.After(time.Second):
		t.Fatal("health ticker never probed the desk during silence")
	}

	// Desk traffic resumes.
	fakeDesk.inbound <- desk.InboundMessage{
		Message: oscmsg.Message{Address: "/Console/Name", Args: []any{"Console"}},
	}

	var healthyEnv, configEnv oscmsg.Envelope
	select {
	case healthyEnv = <-out:
	case <-time.After(time.Second):
		t.Fatal("did not receive status:healthy after desk traffic resumed")
	}
	select {
	case configEnv = <-out:
	case <-time.After(time.Second):
		t.Fatal("did not receive config after recovery")
	}
	if healthyEnv.Kind != oscmsg.KindStatus || healthyEnv.Status == nil || healthyEnv.Status.Desk != string(StatusHealthy) {
		t.Fatalf("post-recovery status envelope = %+v, want status:healthy", healthyEnv)
	}
	if configEnv.Kind != oscmsg.KindConfig || configEnv.Config == nil {
		t.Fatalf("post-recovery envelope = %+v, want config", configEnv)
	}
}

// TestClientMessageDroppedWhileDegraded confirms the plan's Degraded
// behavior ("drop, don't queue — a fader move made while offline has no
// meaningful catch-up semantics"): a client-originated message arriving
// while Degraded must not reach the desk or get broadcast to other clients.
func TestClientMessageDroppedWhileDegraded(t *testing.T) {
	fakeDesk := &scriptedDesk{
		inbound:      make(chan desk.InboundMessage, 64),
		sent:         make(chan oscmsg.Message, 64),
		channelCount: 1, auxCount: 1, groupCount: 1,
		channelNames: map[int]string{1: "Kick"},
		auxNames:     map[int]string{1: "Mon 1"},
		groupNames:   map[int]string{1: "Drums"},
	}
	h := New(fakeDesk, nil, log.New(io.Discard, "", 0), Options{
		StartupRetryInterval: 50 * time.Millisecond,
		HealthCheckInterval:  20 * time.Millisecond,
	})
	clientEvents := make(chan ClientEvent, 8)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go h.Run(ctx, fakeDesk.inbound, clientEvents, nil)

	select {
	case <-h.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("startup did not complete")
	}

	outA := make(chan oscmsg.Envelope, 16)
	outB := make(chan oscmsg.Envelope, 16)
	clientEvents <- ClientEvent{Kind: ClientConnected, ClientID: "A", Outbound: outA}
	clientEvents <- ClientEvent{Kind: ClientConnected, ClientID: "B", Outbound: outB}
	drainN(t, outA, 2)
	drainN(t, outB, 2)
	drainQuiet(outA)
	drainQuiet(outB)

	for _, out := range []chan oscmsg.Envelope{outA, outB} {
		select {
		case env := <-out:
			if env.Kind != oscmsg.KindStatus || env.Status == nil || env.Status.Desk != string(StatusDegraded) {
				t.Fatalf("envelope after silence = %+v, want status:degraded", env)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("did not receive status:degraded — hub never detected the desk timeout")
		}
	}

	// Drain whatever's already queued on fakeDesk.sent — the startup
	// sequence's own queries plus however many health-ticker probes fired
	// while waiting for Degraded above — so the assertion below only sees
	// traffic caused by the moveMsg sent after this point.
	drainAny := time.After(100 * time.Millisecond)
drain:
	for {
		select {
		case <-fakeDesk.sent:
		case <-drainAny:
			break drain
		}
	}

	moveMsg := oscmsg.Message{Address: "/Input_Channels/1/Aux_Send/1/send_level", Args: []any{float32(-6)}}
	clientEvents <- ClientEvent{Kind: ClientMessage, ClientID: "A", Message: moveMsg}

	select {
	case env := <-outB:
		t.Fatalf("client B received %+v — a message sent while Degraded must be dropped, not broadcast", env)
	case <-time.After(200 * time.Millisecond):
	}

	// fakeDesk.sent keeps receiving the health ticker's own unanswered
	// probes throughout this window (every HealthCheckInterval) — those
	// must be ignored; only a non-probe message (i.e. moveMsg leaking
	// through) is a failure.
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case sent := <-fakeDesk.sent:
			if sent.Address != healthProbeAddress {
				t.Fatalf("desk received %+v — a message sent while Degraded must be dropped, not forwarded", sent)
			}
		case <-deadline:
			return
		}
	}
}

// TestAdminApplyLayoutPreservesOrderAndAppendsUnmentioned proves
// AdminApply's Layout field actually reaches buildConfig(): explicitly
// laid-out items (a source, a section marker, another source) come back
// in exactly that order with the marker sitting inline between them, and
// a channel the layout never mentions (channel 1) is appended at the end
// in natural desk order rather than silently dropped — see
// config.LayoutItem's doc comment for why membership/order both come from
// this one sequence, not a separately-stored field.
func TestAdminApplyLayoutPreservesOrderAndAppendsUnmentioned(t *testing.T) {
	fakeDesk := &scriptedDesk{
		inbound:      make(chan desk.InboundMessage, 64),
		channelCount: 3, auxCount: 1, groupCount: 0,
		channelNames: map[int]string{1: "One", 2: "Two", 3: "Three"},
		auxNames:     map[int]string{1: "Mon 1"},
	}
	h := New(fakeDesk, nil, log.New(io.Discard, "", 0), Options{StartupRetryInterval: 50 * time.Millisecond})
	adminEvents := make(chan AdminEvent, 4)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go h.Run(ctx, fakeDesk.inbound, nil, adminEvents)

	select {
	case <-h.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("startup did not complete")
	}

	// Channel 1 is deliberately never mentioned — proving the natural-
	// order fallback for anything the layout doesn't reference.
	adminEvents <- AdminEvent{
		Kind: AdminApply,
		Layout: []config.LayoutItem{
			{Kind: config.LayoutKindSource, Type: "channel", Number: 2, Enabled: true},
			{Kind: config.LayoutKindSection, SectionID: "b", SectionName: "Section B"},
			{Kind: config.LayoutKindSource, Type: "channel", Number: 3, Enabled: false},
		},
	}

	// Same channel as the AdminApply above — FIFO within one channel
	// guarantees the query is processed after the apply lands.
	reply := make(chan oscmsg.ConfigPayload, 1)
	adminEvents <- AdminEvent{Kind: AdminQuery, Reply: reply}

	var cfg oscmsg.ConfigPayload
	select {
	case cfg = <-reply:
	case <-time.After(time.Second):
		t.Fatal("did not receive AdminQuery reply")
	}

	if len(cfg.MixerLayout) != 4 {
		t.Fatalf("mixer layout = %+v, want 4 (channel 2, section marker, channel 3, channel 1 appended)", cfg.MixerLayout)
	}
	if cfg.MixerLayout[0].Kind != oscmsg.LayoutKindSource || cfg.MixerLayout[0].Number != 2 || !cfg.MixerLayout[0].Enabled {
		t.Fatalf("item 0 = %+v, want enabled channel 2", cfg.MixerLayout[0])
	}
	if cfg.MixerLayout[1].Kind != oscmsg.LayoutKindSection || cfg.MixerLayout[1].SectionName != "Section B" {
		t.Fatalf("item 1 = %+v, want the Section B marker, in position between channels 2 and 3", cfg.MixerLayout[1])
	}
	if cfg.MixerLayout[2].Kind != oscmsg.LayoutKindSource || cfg.MixerLayout[2].Number != 3 || cfg.MixerLayout[2].Enabled {
		t.Fatalf("item 2 = %+v, want disabled channel 3", cfg.MixerLayout[2])
	}
	if cfg.MixerLayout[3].Kind != oscmsg.LayoutKindSource || cfg.MixerLayout[3].Number != 1 || !cfg.MixerLayout[3].Enabled {
		t.Fatalf("item 3 = %+v, want channel 1 appended at the end (never mentioned in the applied layout), enabled by default", cfg.MixerLayout[3])
	}
}

// TestAutoHidePatternsHideUnmentionedMatches proves config.Global's
// AutoHideChannelPatterns/AutoHideGroupPatterns/AutoHideAuxPatterns (see
// their doc comment) only change the natural-order fallback's default,
// scoped strictly per source type: a group whose name matches but is
// explicitly laid out with Enabled: true stays enabled (explicit beats the
// pattern), and an aux named "Hall" being hidden must not also hide a
// channel named "Hall Ret" even though it contains the same substring —
// the exact false-positive this per-type scoping exists to prevent.
func TestAutoHidePatternsHideUnmentionedMatches(t *testing.T) {
	fakeDesk := &scriptedDesk{
		inbound:      make(chan desk.InboundMessage, 64),
		channelCount: 2, auxCount: 1, groupCount: 2,
		channelNames: map[int]string{1: "Kick", 2: "Hall Ret"},
		auxNames:     map[int]string{1: "Hall"},
		groupNames:   map[int]string{1: "Master", 2: "Drums"},
	}
	h := New(fakeDesk, nil, log.New(io.Discard, "", 0), Options{StartupRetryInterval: 50 * time.Millisecond})
	adminEvents := make(chan AdminEvent, 4)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go h.Run(ctx, fakeDesk.inbound, nil, adminEvents)

	select {
	case <-h.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("startup did not complete")
	}

	// Group 1 ("Master") is explicitly laid out and enabled, despite
	// matching the group pattern below — explicit always wins. Group 2
	// ("Drums"), both channels, and the aux ("Hall") are all left to the
	// fallback.
	adminEvents <- AdminEvent{
		Kind: AdminApply,
		Layout: []config.LayoutItem{
			{Kind: config.LayoutKindSource, Type: "group", Number: 1, Enabled: true},
		},
		AutoHideGroupPatterns: []string{"master"},
		AutoHideAuxPatterns:   []string{"hall"},
	}

	reply := make(chan oscmsg.ConfigPayload, 1)
	adminEvents <- AdminEvent{Kind: AdminQuery, Reply: reply}

	var cfg oscmsg.ConfigPayload
	select {
	case cfg = <-reply:
	case <-time.After(time.Second):
		t.Fatal("did not receive AdminQuery reply")
	}

	bySource := map[string]oscmsg.LayoutItem{}
	for _, it := range cfg.MixerLayout {
		if it.Kind == oscmsg.LayoutKindSource {
			bySource[sourceKey(it.Type, it.Number)] = it
		}
	}
	if it := bySource[sourceKey("group", 1)]; !it.Enabled {
		t.Errorf("group 1 (Master, explicitly enabled in layout) = %+v, want enabled — explicit overrides should beat a later pattern match", it)
	}
	if it := bySource[sourceKey("group", 2)]; !it.Enabled {
		t.Errorf("group 2 (Drums, unmentioned) = %+v, want enabled — name doesn't match the group pattern", it)
	}
	if it := bySource[sourceKey("channel", 1)]; !it.Enabled {
		t.Errorf("channel 1 (Kick, unmentioned) = %+v, want enabled — name doesn't match the channel pattern (there is none)", it)
	}
	if it := bySource[sourceKey("channel", 2)]; !it.Enabled {
		t.Errorf("channel 2 (Hall Ret, unmentioned) = %+v, want enabled — the aux pattern \"hall\" must not bleed into channels", it)
	}
	if len(cfg.Auxes) != 1 || cfg.Auxes[0].Enabled {
		t.Errorf("aux 1 (Hall, unmentioned, no aux overrides) = %+v, want hidden by the aux auto-hide pattern", cfg.Auxes)
	}
}

// TestAdminNetworkApplyChangesDeskAddressOnlyWhenChanged proves
// AdminNetworkApply reaches h.desk via SetRemote/Rebind, and — critically
// — only for the field that actually changed: Desk IP/port and OSC
// listen port live on the same Network form, so re-saving it with only an
// unrelated field different must not needlessly rebind the live OSC
// socket or reset the desk address, both of which briefly disrupt real
// traffic (see applyNetworkChange's doc comment).
func TestAdminNetworkApplyChangesDeskAddressOnlyWhenChanged(t *testing.T) {
	fakeDesk := &scriptedDesk{
		inbound:      make(chan desk.InboundMessage, 64),
		channelCount: 1, auxCount: 1, groupCount: 0,
		channelNames: map[int]string{1: "Kick"},
		auxNames:     map[int]string{1: "Mon 1"},
	}
	h := New(fakeDesk, nil, log.New(io.Discard, "", 0), Options{
		StartupRetryInterval: 50 * time.Millisecond,
		DeskIP:               "10.0.0.10",
		DeskPort:             8000,
		OSCPort:              9000,
	})
	adminEvents := make(chan AdminEvent, 4)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go h.Run(ctx, fakeDesk.inbound, nil, adminEvents)

	select {
	case <-h.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("startup did not complete")
	}

	waitForApply := func() {
		reply := make(chan oscmsg.ConfigPayload, 1)
		adminEvents <- AdminEvent{Kind: AdminQuery, Reply: reply}
		select {
		case <-reply:
		case <-time.After(time.Second):
			t.Fatal("did not receive AdminQuery reply")
		}
	}

	// Desk IP/port change, OSC port unchanged — expect SetRemote only.
	adminEvents <- AdminEvent{Kind: AdminNetworkApply, DeskIP: "10.0.0.20", DeskPort: 8001, OSCPort: 9000}
	waitForApply()

	if calls := fakeDesk.remoteCallsSnapshot(); len(calls) != 1 || calls[0] != (remoteCall{ip: "10.0.0.20", port: 8001}) {
		t.Fatalf("SetRemote calls = %+v, want exactly one call with the new desk address", calls)
	}
	if calls := fakeDesk.rebindCallsSnapshot(); len(calls) != 0 {
		t.Fatalf("Rebind calls = %v, want none — OSC port didn't change", calls)
	}

	// Same event again (nothing changed from hub's point of view) — expect
	// no further calls at all.
	adminEvents <- AdminEvent{Kind: AdminNetworkApply, DeskIP: "10.0.0.20", DeskPort: 8001, OSCPort: 9000}
	waitForApply()

	if calls := fakeDesk.remoteCallsSnapshot(); len(calls) != 1 {
		t.Fatalf("SetRemote calls = %+v after a no-op re-save, want still exactly one — an unchanged field must not re-trigger it", calls)
	}
	if calls := fakeDesk.rebindCallsSnapshot(); len(calls) != 0 {
		t.Fatalf("Rebind calls = %v after a no-op re-save, want none", calls)
	}

	// OSC port change only — expect Rebind only, no further SetRemote.
	adminEvents <- AdminEvent{Kind: AdminNetworkApply, DeskIP: "10.0.0.20", DeskPort: 8001, OSCPort: 9001}
	waitForApply()

	if calls := fakeDesk.remoteCallsSnapshot(); len(calls) != 1 {
		t.Fatalf("SetRemote calls = %+v after an OSC-port-only change, want still exactly one", calls)
	}
	if calls := fakeDesk.rebindCallsSnapshot(); len(calls) != 1 || calls[0] != 9001 {
		t.Fatalf("Rebind calls = %v, want exactly one call with the new OSC port", calls)
	}
}
