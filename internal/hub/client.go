package hub

import (
	"strings"

	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
)

// ClientEventKind distinguishes the lifecycle events a WS client produces,
// forwarded into hub's single serialization point (see plan: "Concurrency
// model" — a tagged union processed one at a time, here realized as a
// second channel merged via select in Run rather than a wrapper interface,
// since Go's select already is the tagged-dispatch mechanism).
type ClientEventKind int

const (
	ClientConnected ClientEventKind = iota
	ClientDisconnected
	ClientMessage
)

// ClientEvent is what internal/wsserver sends into the hub for every
// lifecycle event or inbound message on a WS connection.
type ClientEvent struct {
	Kind     ClientEventKind
	ClientID string
	Outbound chan<- oscmsg.Envelope // set for ClientConnected only
	Message  oscmsg.Message         // set for ClientMessage only
}

func (h *Hub) handleClientEvent(ev ClientEvent) {
	switch ev.Kind {
	case ClientConnected:
		h.clients[ev.ClientID] = ev.Outbound
		h.log.Printf("client connected: %s (total %d)", ev.ClientID, len(h.clients))
		h.sendTo(ev.ClientID, ev.Outbound, oscmsg.NewStatusEnvelope(string(h.status)))
		h.sendTo(ev.ClientID, ev.Outbound, oscmsg.NewConfigEnvelope(h.buildConfig()))

	case ClientDisconnected:
		delete(h.clients, ev.ClientID)
		delete(h.dropStreak, ev.ClientID)
		h.log.Printf("client disconnected: %s (total %d)", ev.ClientID, len(h.clients))

	case ClientMessage:
		h.handleClientMessage(ev.ClientID, ev.Message)
	}
}

// handleClientMessage is the Go port of the JS version's WS "message"
// handler (index.js): run the pipeline, answer a "/address/?" query from
// cache directly to the requester instead of broadcasting, otherwise cache
// the new value and fan it out — unconditionally, not gated by whether the
// value matches cache (see handleDeskMessage's doc comment for why a
// value-dedup must never gate relay).
//
// Dropped outright while Starting, Reloading, or Degraded: a channel/aux
// number a client is acting on may no longer mean the same thing once a
// different session has loaded (or hasn't finished loading yet), and while
// Degraded there is no live desk to send it to — there's no safe target to
// forward a stale action to in any of these states, so drop rather than
// queue or misdirect it (plan's Degraded behavior: "drop, don't queue — a
// fader move made while offline has no meaningful catch-up semantics").
func (h *Hub) handleClientMessage(clientID string, msg oscmsg.Message) {
	// Guarded at the call site, not just inside debugf: Go evaluates a
	// variadic call's arguments (and boxes each into the []any slice)
	// unconditionally, before debugf ever gets a chance to no-op internally
	// — on this hot path (every client message), that cost is paid on every
	// call regardless of whether Debug is on. Confirmed via allocation
	// profiling: with Debug off, these unguarded calls were the majority of
	// all allocations in the message-handling hot path (see broadcast's
	// debugf calls, the worse offender — external-device loop runs one per
	// registered device, per message).
	if h.debug {
		h.debugf("client-path %s from %s", msg.Address, clientID)
	}

	if h.status == StatusStarting || h.status == StatusReloading || h.status == StatusDegraded {
		if h.debug {
			h.debugf("  status=%s: dropping client message", h.status)
		}
		return
	}

	out, ok := h.pipeline.Run(msg)
	if !ok {
		return
	}
	msg = out

	if strings.HasSuffix(msg.Address, "/?") {
		key := strings.TrimSuffix(msg.Address, "/?")
		if cached, ok := h.cache[key]; ok {
			if clientOut, ok := h.clients[clientID]; ok {
				h.sendTo(clientID, clientOut, oscmsg.NewOSCEnvelope(cached))
			}
			return
		}
	}

	h.maybeCache(msg)
	h.broadcast(msg, clientSource(clientID))
}

// maxConsecutiveDrops bounds how long sendTo tolerates a client's outbound
// channel being full before giving up on it — see sendTo's doc comment.
// Chosen well above outboundBufferSize's own headroom (a transient burst
// briefly filling the channel is normal and must not trip this) but far
// below what it'd take to notice: a client stuck at this streak has gone
// long enough without draining a single message that it is not coming
// back on its own.
const maxConsecutiveDrops = 64

// sendTo delivers one envelope to a client's outbound channel without ever
// blocking the hub goroutine — a wedged/backgrounded client's channel
// filling up must not stall fan-out to everyone else (see plan:
// "Backpressure policy"). A single full channel is expected and cheap to
// drop-and-move-on; what isn't cheap is doing that on every message for a
// client that's stopped draining entirely — logging every one of those
// (the previous behavior) turned a backgrounded mobile tab into a
// synchronous log write on hub's hot path for every single desk/client
// message thereafter, which is what was pinning CPU on real hardware
// during heavy fader movement, not the message volume itself. Past
// maxConsecutiveDrops in a row, close the channel and drop the client
// instead of logging forever — closing is what tells wsserver's write pump
// to exit and tear the connection down (see writePump), and mixer.js's
// existing auto-reconnect brings it straight back once it can keep up.
func (h *Hub) sendTo(id string, out chan<- oscmsg.Envelope, env oscmsg.Envelope) {
	select {
	case out <- env:
		delete(h.dropStreak, id)
	default:
		h.dropStreak[id]++
		if h.dropStreak[id] < maxConsecutiveDrops {
			return
		}
		h.log.Printf("client %s: outbound channel stuck full after %d messages, disconnecting", id, h.dropStreak[id])
		delete(h.dropStreak, id)
		delete(h.clients, id)
		close(out)
	}
}

// broadcast is the Go port of the JS version's broadcast(oscMsg, source):
// relay to the desk (unless the desk itself was the source), to configured
// external OSC devices (excluding the one that sent it, unless its
// Loopback flag says otherwise), and to every other connected WS client.
//
// A desk-path source carries the real UDP sender IP (see broadcastSource's
// doc comment) — critically, that IP is NOT necessarily the desk's: an
// external device that also sends to our shared OSC socket (confirmed
// live — a registered external device's traffic) looks identical at the
// transport layer to a real desk reply, and the two must be told apart by
// comparing against h.deskIP, exactly as JS compares info.address against
// config.desk.ip. Getting this wrong (as an earlier version of this code
// did, unconditionally treating every desk-path message as "from the
// desk") silently drops anything an external device sends inbound — it
// never reaches the console.
func (h *Hub) broadcast(msg oscmsg.Message, source broadcastSource) {
	fromDesk := source.clientID == "" && source.ip == h.deskIP
	// if h.debug guards here (not just inside debugf) — see
	// handleClientMessage's doc comment: this is the single hottest call in
	// the app (every desk AND client message passes through here), and an
	// unguarded variadic debugf call pays argument-boxing allocation cost
	// even when the result is immediately discarded.
	if h.debug {
		h.debugf("broadcast %s: source.ip=%q source.clientID=%q deskIP=%q fromDesk=%v externalDevices=%d",
			msg.Address, source.ip, source.clientID, h.deskIP, fromDesk, len(h.externalDevices))
	}
	if !fromDesk {
		if err := h.desk.Send(msg); err != nil {
			h.log.Printf("broadcast to desk: %v", err)
		} else if h.debug {
			h.debugf("  -> desk: sent")
		}
	}

	for _, ext := range h.externalDevices {
		if !ext.Broadcast {
			if h.debug {
				h.debugf("  -> external %q: skipped (broadcast disabled)", ext.Name)
			}
			continue
		}
		// Don't echo a message back to the same external device that sent
		// it, unless it explicitly asked to receive its own loopback.
		// source.ip is empty for client-path messages, which can never
		// equal a real device's IP, so this naturally never excludes
		// anything for that path — matching JS's WS-object-vs-string
		// comparison always being unequal there.
		if !ext.Loopback && source.ip != "" && ext.Addr.IP.String() == source.ip {
			if h.debug {
				h.debugf("  -> external %q: skipped (loopback source)", ext.Name)
			}
			continue
		}
		if err := h.desk.SendTo(msg, ext.Addr); err != nil {
			h.log.Printf("broadcast to external %q: %v", ext.Name, err)
		} else if h.debug {
			h.debugf("  -> external %q at %s: sent", ext.Name, ext.Addr)
		}
	}

	env := oscmsg.NewOSCEnvelope(msg)
	for id, out := range h.clients {
		if id == source.clientID {
			continue
		}
		h.sendTo(id, out, env)
	}
}
