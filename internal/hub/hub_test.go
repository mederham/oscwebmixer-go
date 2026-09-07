package hub

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mederham/oscwebmixer-go/internal/config"
	"github.com/mederham/oscwebmixer-go/internal/desk"
	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
	"github.com/mederham/oscwebmixer-go/internal/pipeline"
)

type scriptedDesk struct {
	inbound      chan desk.InboundMessage
	sent         chan oscmsg.Message
	sentExternal chan externalSend

	channelCount, auxCount, groupCount int
	channelNames, auxNames, groupNames map[int]string
	channelStereo                      map[int]bool

	deskIP string

	mu       sync.Mutex
	dropOnce map[string]bool

	remoteCalls  []remoteCall
	rebindCalls  []int
	setRemoteErr error
	rebindErr    error
}

type remoteCall struct {
	ip   string
	port int
}

func (d *scriptedDesk) simulateDroppedReply(address string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dropOnce == nil {
		d.dropOnce = make(map[string]bool)
	}
	d.dropOnce[address] = true
}

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
		return nil
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

type externalSend struct {
	Message oscmsg.Message
	Addr    *net.UDPAddr
}

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
	var from *net.UDPAddr
	if d.deskIP != "" {
		from = &net.UDPAddr{IP: net.ParseIP(d.deskIP), Port: 9000}
	}
	d.inbound <- desk.InboundMessage{Message: oscmsg.Message{Address: address, Args: args}, From: from}
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
		groupNames:   map[int]string{2: "Drums"},
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
		groupNames:   map[int]string{},
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

	cached, ok := h.cache["/Group_Outputs/1/Buss_Trim/name"]
	if !ok {
		t.Fatal("unnamed group's name address was not cached at all")
	}
	if len(cached.Args) != 0 {
		t.Fatalf("args = %v, want empty (unnamed bus)", cached.Args)
	}
}

type countingInterceptor struct{ n atomic.Int64 }

func (c *countingInterceptor) Handle(msg oscmsg.Message) (oscmsg.Message, bool) {
	c.n.Add(1)
	return msg, true
}

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
	afterStartup := waitForQuiet(counter, 2*time.Second, 20*time.Millisecond)

	fakeDesk.inbound <- desk.InboundMessage{
		Message: oscmsg.Message{Address: "/Input_Channels/1/Channel_Input/name", Args: []any{"Kick"}},
	}
	fakeDesk.inbound <- desk.InboundMessage{
		Message: oscmsg.Message{Address: "/Input_Channels/1/Channel_Input/name", Args: []any{"Renamed"}},
	}

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

const (
	quietPollInterval = time.Millisecond
	quietWindow       = 20 * time.Millisecond
)

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
	h := New(fakeDesk, nil, log.New(io.Discard, "", 0), Options{
		StartupRetryInterval: 50 * time.Millisecond,
		InitialLayouts:       map[string]config.NamedLayout{config.DefaultLayoutID: {Name: "Default"}},
	})
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

func TestAdvanceStartupDoesNotResendPendingQuery(t *testing.T) {
	fakeDesk := &scriptedDesk{
		inbound:      make(chan desk.InboundMessage, 64),
		sent:         make(chan oscmsg.Message, 64),
		channelCount: 1, auxCount: 1, groupCount: 1,
		channelNames: map[int]string{1: "Kick"},
		auxNames:     map[int]string{1: "Mon 1"},
		groupNames:   map[int]string{1: "Drums"},
	}
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
	if len(configEnv.Config.MixerLayouts["default"].Items) != 2 {
		t.Fatalf("mixer layout = %+v, want 2 (1 channel + 1 group)", configEnv.Config.MixerLayouts["default"].Items)
	}
	if configEnv.Config.MixerLayouts["default"].Items[0].Type != "channel" || configEnv.Config.MixerLayouts["default"].Items[1].Type != "group" {
		t.Fatalf("layout order = %+v, want channel then group (natural desk order fallback)", configEnv.Config.MixerLayouts["default"].Items)
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

func TestChronicallyFullClientGetsDisconnected(t *testing.T) {
	fakeDesk := &scriptedDesk{
		inbound:      make(chan desk.InboundMessage, 64),
		sent:         make(chan oscmsg.Message, 4096),
		channelCount: 1, auxCount: 1, groupCount: 1,
		channelNames: map[int]string{1: "Kick"},
		auxNames:     map[int]string{1: "Mon 1"},
		groupNames:   map[int]string{1: "Drums"},
	}
	h := New(fakeDesk, nil, log.New(io.Discard, "", 0), Options{StartupRetryInterval: 50 * time.Millisecond})
	clientEvents := make(chan ClientEvent, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	go h.Run(ctx, fakeDesk.inbound, clientEvents, nil)
	select {
	case <-h.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("startup did not complete")
	}

	sender := make(chan oscmsg.Envelope, 8)
	stuck := make(chan oscmsg.Envelope, 4)
	clientEvents <- ClientEvent{Kind: ClientConnected, ClientID: "sender", Outbound: sender}
	clientEvents <- ClientEvent{Kind: ClientConnected, ClientID: "stuck", Outbound: stuck}
	drainN(t, sender, 2)
	drainQuiet(sender)

	moveMsg := oscmsg.Message{Address: "/Input_Channels/1/Aux_Send/1/send_level", Args: []any{float32(-6)}}
	for i := 0; i < maxConsecutiveDrops*3; i++ {
		clientEvents <- ClientEvent{Kind: ClientMessage, ClientID: "sender", Message: moveMsg}
	}

	closed := false
	deadline := time.After(5 * time.Second)
readLoop:
	for {
		select {
		case _, ok := <-stuck:
			if !ok {
				closed = true
				break readLoop
			}
		case <-deadline:
			break readLoop
		}
	}
	if !closed {
		t.Fatal("stuck client's outbound channel was never closed")
	}

	other := make(chan oscmsg.Envelope, 8)
	clientEvents <- ClientEvent{Kind: ClientConnected, ClientID: "other", Outbound: other}
	drainN(t, other, 2)
	clientEvents <- ClientEvent{Kind: ClientMessage, ClientID: "sender", Message: moveMsg}
	select {
	case env := <-other:
		if env.Kind != oscmsg.KindOSC || env.Message == nil || env.Message.Address != moveMsg.Address {
			t.Fatalf("other received %+v, want the fader move", env)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("hub stopped broadcasting to other clients after disconnecting the stuck one")
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

func TestExternalDeviceMessagesAreForwardedToDesk(t *testing.T) {
	const deskIP = "10.0.0.18"
	const ipadIP = "10.0.0.12"

	fakeDesk := &scriptedDesk{
		inbound:      make(chan desk.InboundMessage, 64),
		sent:         make(chan oscmsg.Message, 64),
		deskIP:       deskIP,
		channelCount: 1, auxCount: 1, groupCount: 1,
		channelNames: map[int]string{1: "Kick"},
		auxNames:     map[int]string{1: "Mon 1"},
		groupNames:   map[int]string{1: "Drums"},
	}
	h := New(fakeDesk, nil, log.New(io.Discard, "", 0), Options{
		DeskIP: deskIP, StartupRetryInterval: 50 * time.Millisecond,
		ExternalDevices: []ExternalDevice{
			{Name: "iPad", Addr: &net.UDPAddr{IP: net.ParseIP(ipadIP), Port: 9000}, Broadcast: true},
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

func TestUnregisteredSourceMessagesAreDropped(t *testing.T) {
	const deskIP = "10.0.0.18"
	const strangerIP = "10.0.0.99"

	fakeDesk := &scriptedDesk{
		inbound:      make(chan desk.InboundMessage, 64),
		sent:         make(chan oscmsg.Message, 64),
		deskIP:       deskIP,
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
		Message: oscmsg.Message{Address: "/Console/Name/?", Args: []any{}},
		From:    &net.UDPAddr{IP: net.ParseIP(strangerIP), Port: 9000},
	}

	select {
	case sent := <-fakeDesk.sent:
		t.Fatalf("desk received a message from an unregistered source: %+v", sent)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestExternalDeviceMessagesAreDroppedWhenBroadcastDisabled(t *testing.T) {
	const deskIP = "10.0.0.18"
	const ipadIP = "10.0.0.12"

	fakeDesk := &scriptedDesk{
		inbound:      make(chan desk.InboundMessage, 64),
		sent:         make(chan oscmsg.Message, 64),
		deskIP:       deskIP,
		channelCount: 1, auxCount: 1, groupCount: 1,
		channelNames: map[int]string{1: "Kick"},
		auxNames:     map[int]string{1: "Mon 1"},
		groupNames:   map[int]string{1: "Drums"},
	}
	h := New(fakeDesk, nil, log.New(io.Discard, "", 0), Options{
		DeskIP: deskIP, StartupRetryInterval: 50 * time.Millisecond,
		ExternalDevices: []ExternalDevice{
			{Name: "iPad", Addr: &net.UDPAddr{IP: net.ParseIP(ipadIP), Port: 9000}, Broadcast: false},
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

	fakeDesk.inbound <- desk.InboundMessage{
		Message: oscmsg.Message{Address: "/Console/Name/?", Args: []any{}},
		From:    &net.UDPAddr{IP: net.ParseIP(ipadIP), Port: 9000},
	}

	select {
	case sent := <-fakeDesk.sent:
		t.Fatalf("desk received a message from a broadcast-disabled device: %+v", sent)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestDeskRepliesAreNotEchoedBackToDesk(t *testing.T) {
	const deskIP = "10.0.0.18"

	fakeDesk := &scriptedDesk{
		inbound:      make(chan desk.InboundMessage, 64),
		sent:         make(chan oscmsg.Message, 64),
		deskIP:       deskIP,
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

func TestExternalDeviceLoopbackExclusion(t *testing.T) {
	const deskIP = "10.0.0.18"
	const ipadIP = "10.0.0.12"
	const otherIP = "10.0.0.50"

	fakeDesk := &scriptedDesk{
		inbound:      make(chan desk.InboundMessage, 64),
		sent:         make(chan oscmsg.Message, 64),
		sentExternal: make(chan externalSend, 64),
		deskIP:       deskIP,
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

func TestDeskEchoSuppressedShortlyAfterClientDrivenChange(t *testing.T) {
	const deskIP = "10.0.0.18"
	const addr = "/Some/Fader/Value"

	fakeDesk := &scriptedDesk{
		inbound: make(chan desk.InboundMessage, 64),
		sent:    make(chan oscmsg.Message, 64),
		deskIP:  deskIP,
	}
	h := New(fakeDesk, nil, log.New(io.Discard, "", 0), Options{
		DeskIP: deskIP, StartupRetryInterval: 50 * time.Millisecond,
	})
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
	drainAllSent(fakeDesk)

	clientEvents <- ClientEvent{Kind: ClientMessage, ClientID: "A", Message: oscmsg.Message{Address: addr, Args: []any{float32(-6)}}}
	select {
	case env := <-outB:
		if env.Kind != oscmsg.KindOSC || env.Message == nil || env.Message.Address != addr {
			t.Fatalf("relayed envelope = %+v, want an OSC message for %s", env, addr)
		}
	case <-time.After(time.Second):
		t.Fatal("B never received the client-driven relay")
	}
	drainAllSent(fakeDesk)

	fakeDesk.inbound <- desk.InboundMessage{
		Message: oscmsg.Message{Address: addr, Args: []any{float32(-6.02)}},
		From:    &net.UDPAddr{IP: net.ParseIP(deskIP), Port: 9000},
	}
	select {
	case env := <-outB:
		t.Fatalf("B received a second envelope %+v — the desk's echo of a just-client-driven address must be suppressed", env)
	case <-time.After(200 * time.Millisecond):
	}

	time.Sleep(deskEchoSuppressWindow)
	fakeDesk.inbound <- desk.InboundMessage{
		Message: oscmsg.Message{Address: addr, Args: []any{float32(-6.02)}},
		From:    &net.UDPAddr{IP: net.ParseIP(deskIP), Port: 9000},
	}
	select {
	case env := <-outB:
		if env.Kind != oscmsg.KindOSC || env.Message == nil || env.Message.Address != addr {
			t.Fatalf("post-window envelope = %+v, want an OSC message for %s", env, addr)
		}
	case <-time.After(time.Second):
		t.Fatal("B never received the desk's echo once the suppression window elapsed")
	}
}

func TestExternalDeviceGetsReplyEvenWhenValueMatchesCache(t *testing.T) {
	const deskIP = "10.0.0.18"
	const ipadIP = "10.0.0.12"

	fakeDesk := &scriptedDesk{
		inbound:      make(chan desk.InboundMessage, 64),
		sentExternal: make(chan externalSend, 64),
		deskIP:       deskIP,
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

	fakeDesk.inbound <- desk.InboundMessage{
		Message: oscmsg.Message{Address: "/Console/Channels/?", Args: []any{}},
		From:    &net.UDPAddr{IP: net.ParseIP(ipadIP), Port: 9000},
	}

	deadline := time.After(time.Second)
	for {
		select {
		case sent := <-fakeDesk.sentExternal:
			if sent.Message.Address == "/Console/Input_Channels" && sent.Addr.IP.String() == ipadIP {
				return
			}
		case <-deadline:
			t.Fatal("the iPad's query for an already-cached value never got a relayed reply")
		}
	}
}

func TestSessionChangeReQueriesFreshTopology(t *testing.T) {
	fakeDesk := &scriptedDesk{
		inbound:      make(chan desk.InboundMessage, 64),
		channelCount: 1, auxCount: 1, groupCount: 1,
		channelNames: map[int]string{1: "Kick"},
		auxNames:     map[int]string{1: "Mon 1"},
		groupNames:   map[int]string{1: "Drums"},
	}
	h := New(fakeDesk, nil, log.New(io.Discard, "", 0), Options{
		StartupRetryInterval: 50 * time.Millisecond,
		InitialLayouts:       map[string]config.NamedLayout{config.DefaultLayoutID: {Name: "Default"}},
	})
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
	drainN(t, out, 2)
	drainQuiet(out)

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

	sources := configEnv.Config.MixerLayouts["default"].Items
	if len(sources) != 3 {
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

func TestSessionChangeDropsClientMessages(t *testing.T) {
	fakeDesk := &scriptedDesk{
		inbound:      make(chan desk.InboundMessage, 64),
		sent:         make(chan oscmsg.Message, 64),
		channelCount: 1, auxCount: 1, groupCount: 1,
		channelNames: map[int]string{1: "Kick"},
		auxNames:     map[int]string{1: "Mon 1"},
		groupNames:   map[int]string{1: "Drums"},
	}
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
	drainQuiet(outA)
	drainQuiet(outB)
	drainAllSent(fakeDesk)

	fakeDesk.simulateDroppedReply("/Console/Channels/?")
	fakeDesk.inbound <- desk.InboundMessage{
		Message: oscmsg.Message{Address: sessionChangeAddress, Args: []any{int32(0)}},
	}

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
	drainAllSent(fakeDesk)

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
	drainAllSent(fakeDesk)

	var degradedEnv oscmsg.Envelope
	select {
	case degradedEnv = <-out:
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive status:degraded after desk silence")
	}
	if degradedEnv.Kind != oscmsg.KindStatus || degradedEnv.Status == nil || degradedEnv.Status.Desk != string(StatusDegraded) {
		t.Fatalf("envelope after silence = %+v, want status:degraded", degradedEnv)
	}

	select {
	case sent := <-fakeDesk.sent:
		if sent.Address != healthProbeAddress {
			t.Fatalf("query sent during silence = %q, want %q", sent.Address, healthProbeAddress)
		}
	case <-time.After(time.Second):
		t.Fatal("health ticker never probed the desk during silence")
	}

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

	adminEvents <- AdminEvent{
		Kind: AdminApply,
		Layouts: map[string]config.NamedLayout{
			config.DefaultLayoutID: {
				Name: "Default",
				Items: []config.LayoutItem{
					{Kind: config.LayoutKindSource, Type: "channel", Number: 2, Enabled: true},
					{Kind: config.LayoutKindSection, SectionID: "b", SectionName: "Section B"},
					{Kind: config.LayoutKindSource, Type: "channel", Number: 3, Enabled: false},
				},
			},
		},
	}

	reply := make(chan oscmsg.ConfigPayload, 1)
	adminEvents <- AdminEvent{Kind: AdminQuery, Reply: reply}

	var cfg oscmsg.ConfigPayload
	select {
	case cfg = <-reply:
	case <-time.After(time.Second):
		t.Fatal("did not receive AdminQuery reply")
	}

	if len(cfg.MixerLayouts["default"].Items) != 4 {
		t.Fatalf("mixer layout = %+v, want 4 (channel 2, section marker, channel 3, channel 1 appended)", cfg.MixerLayouts["default"].Items)
	}
	if cfg.MixerLayouts["default"].Items[0].Kind != oscmsg.LayoutKindSource || cfg.MixerLayouts["default"].Items[0].Number != 2 || !cfg.MixerLayouts["default"].Items[0].Enabled {
		t.Fatalf("item 0 = %+v, want enabled channel 2", cfg.MixerLayouts["default"].Items[0])
	}
	if cfg.MixerLayouts["default"].Items[1].Kind != oscmsg.LayoutKindSection || cfg.MixerLayouts["default"].Items[1].SectionName != "Section B" {
		t.Fatalf("item 1 = %+v, want the Section B marker, in position between channels 2 and 3", cfg.MixerLayouts["default"].Items[1])
	}
	if cfg.MixerLayouts["default"].Items[2].Kind != oscmsg.LayoutKindSource || cfg.MixerLayouts["default"].Items[2].Number != 3 || cfg.MixerLayouts["default"].Items[2].Enabled {
		t.Fatalf("item 2 = %+v, want disabled channel 3", cfg.MixerLayouts["default"].Items[2])
	}
	if cfg.MixerLayouts["default"].Items[3].Kind != oscmsg.LayoutKindSource || cfg.MixerLayouts["default"].Items[3].Number != 1 || !cfg.MixerLayouts["default"].Items[3].Enabled {
		t.Fatalf("item 3 = %+v, want channel 1 appended at the end (never mentioned in the applied layout), enabled by default", cfg.MixerLayouts["default"].Items[3])
	}
}

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

	adminEvents <- AdminEvent{
		Kind: AdminApply,
		Layouts: map[string]config.NamedLayout{
			config.DefaultLayoutID: {
				Name:  "Default",
				Items: []config.LayoutItem{{Kind: config.LayoutKindSource, Type: "group", Number: 1, Enabled: true}},
			},
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
	for _, it := range cfg.MixerLayouts["default"].Items {
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

func TestAdminStatusQueryReportsNeverSeenBeforeAnyDeskTraffic(t *testing.T) {
	fakeDesk := &scriptedDesk{inbound: make(chan desk.InboundMessage, 64)}
	h := New(fakeDesk, nil, log.New(io.Discard, "", 0), Options{DeskIP: "10.0.0.5"})

	reply := make(chan DeskStatusInfo, 1)
	h.handleAdminEvent(AdminEvent{Kind: AdminStatusQuery, StatusReply: reply})
	if info := <-reply; !info.LastSeen.IsZero() {
		t.Fatalf("LastSeen = %v, want the zero time before any desk traffic has arrived", info.LastSeen)
	}

	h.handleDeskMessage(oscmsg.Message{Address: "/Console/Name", Args: []any{"Test Desk"}}, "10.0.0.5")

	reply2 := make(chan DeskStatusInfo, 1)
	h.handleAdminEvent(AdminEvent{Kind: AdminStatusQuery, StatusReply: reply2})
	if info := <-reply2; info.LastSeen.IsZero() {
		t.Fatal("LastSeen is still zero after real desk traffic arrived")
	}
}

func TestAdminNetworkApplyChangesDeskAddressOnlyWhenChanged(t *testing.T) {
	fakeDesk := &scriptedDesk{
		inbound:      make(chan desk.InboundMessage, 64),
		deskIP:       "10.0.0.10",
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

	adminEvents <- AdminEvent{Kind: AdminNetworkApply, DeskIP: "10.0.0.20", DeskPort: 8001, OSCPort: 9000}
	waitForApply()

	if calls := fakeDesk.remoteCallsSnapshot(); len(calls) != 1 || calls[0] != (remoteCall{ip: "10.0.0.20", port: 8001}) {
		t.Fatalf("SetRemote calls = %+v, want exactly one call with the new desk address", calls)
	}
	if calls := fakeDesk.rebindCallsSnapshot(); len(calls) != 0 {
		t.Fatalf("Rebind calls = %v, want none — OSC port didn't change", calls)
	}

	adminEvents <- AdminEvent{Kind: AdminNetworkApply, DeskIP: "10.0.0.20", DeskPort: 8001, OSCPort: 9000}
	waitForApply()

	if calls := fakeDesk.remoteCallsSnapshot(); len(calls) != 1 {
		t.Fatalf("SetRemote calls = %+v after a no-op re-save, want still exactly one — an unchanged field must not re-trigger it", calls)
	}
	if calls := fakeDesk.rebindCallsSnapshot(); len(calls) != 0 {
		t.Fatalf("Rebind calls = %v after a no-op re-save, want none", calls)
	}

	adminEvents <- AdminEvent{Kind: AdminNetworkApply, DeskIP: "10.0.0.20", DeskPort: 8001, OSCPort: 9001}
	waitForApply()

	if calls := fakeDesk.remoteCallsSnapshot(); len(calls) != 1 {
		t.Fatalf("SetRemote calls = %+v after an OSC-port-only change, want still exactly one", calls)
	}
	if calls := fakeDesk.rebindCallsSnapshot(); len(calls) != 1 || calls[0] != 9001 {
		t.Fatalf("Rebind calls = %v, want exactly one call with the new OSC port", calls)
	}
}

func TestLogSendErrThrottledSuppressesRepeats(t *testing.T) {
	var buf bytes.Buffer
	h := New(&scriptedDesk{}, nil, log.New(&buf, "", 0), Options{})

	for i := 0; i < 5; i++ {
		h.logSendErrThrottled("dead-device", "broadcast to external %q: boom", "dead-device")
	}
	if n := strings.Count(buf.String(), "boom"); n != 1 {
		t.Fatalf("logged %d times within the throttle window, want exactly 1: %s", n, buf.String())
	}

	h.logSendErrThrottled("other-device", "broadcast to external %q: boom", "other-device")
	if n := strings.Count(buf.String(), "boom"); n != 2 {
		t.Fatalf("logged %d times after a different key, want exactly 2: %s", n, buf.String())
	}

	h.lastSendErrLogged["dead-device"] = time.Now().Add(-sendErrLogInterval - time.Second)
	h.logSendErrThrottled("dead-device", "broadcast to external %q: boom", "dead-device")
	if n := strings.Count(buf.String(), "boom"); n != 3 {
		t.Fatalf("logged %d times after the throttle window elapsed, want exactly 3: %s", n, buf.String())
	}
}

func TestBuildConfigPerLayoutIndependence(t *testing.T) {
	fakeDesk := &scriptedDesk{
		inbound:      make(chan desk.InboundMessage, 64),
		channelCount: 2, auxCount: 1, groupCount: 0,
		channelNames: map[int]string{1: "One", 2: "Two"},
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

	adminEvents <- AdminEvent{
		Kind: AdminApply,
		Layouts: map[string]config.NamedLayout{
			config.DefaultLayoutID: {
				Name:  "Default",
				Items: []config.LayoutItem{{Kind: config.LayoutKindSource, Type: "channel", Number: 1, Enabled: true}},
			},
			"mon-1": {
				Name:  "Monitor Mix",
				Items: []config.LayoutItem{{Kind: config.LayoutKindSource, Type: "channel", Number: 2, Enabled: false}},
			},
		},
		Auxes: []config.Aux{{Enabled: true, LayoutID: "mon-1"}},
	}

	reply := make(chan oscmsg.ConfigPayload, 1)
	adminEvents <- AdminEvent{Kind: AdminQuery, Reply: reply}

	var cfg oscmsg.ConfigPayload
	select {
	case cfg = <-reply:
	case <-time.After(time.Second):
		t.Fatal("did not receive AdminQuery reply")
	}

	if len(cfg.MixerLayouts) != 2 {
		t.Fatalf("MixerLayouts = %+v, want 2 entries", cfg.MixerLayouts)
	}

	def := cfg.MixerLayouts[config.DefaultLayoutID]
	if len(def.Items) != 2 || def.Items[0].Number != 1 || !def.Items[0].Enabled {
		t.Fatalf("default layout = %+v, want channel 1 enabled first", def.Items)
	}
	if def.Items[1].Number != 2 || !def.Items[1].Enabled {
		t.Fatalf("default layout = %+v, want channel 2 appended (unmentioned), enabled by default", def.Items)
	}

	mon := cfg.MixerLayouts["mon-1"]
	if mon.Name != "Monitor Mix" {
		t.Fatalf("mon-1 name = %q, want Monitor Mix", mon.Name)
	}
	if len(mon.Items) != 2 || mon.Items[0].Number != 2 || mon.Items[0].Enabled {
		t.Fatalf("mon-1 layout = %+v, want channel 2 disabled first (independent of default's order/enabled)", mon.Items)
	}
	if mon.Items[1].Number != 1 || !mon.Items[1].Enabled {
		t.Fatalf("mon-1 layout = %+v, want channel 1 appended (unmentioned in mon-1), enabled by default", mon.Items)
	}

	if len(cfg.Auxes) != 1 || cfg.Auxes[0].LayoutID != "mon-1" {
		t.Fatalf("auxes = %+v, want aux 1 layoutId=mon-1", cfg.Auxes)
	}
}
