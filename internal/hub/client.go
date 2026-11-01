package hub

import (
	"strings"
	"time"

	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
)

const sendErrLogInterval = 1 * time.Minute

const deskEchoSuppressWindow = 300 * time.Millisecond

type ClientEventKind int

const (
	ClientConnected ClientEventKind = iota
	ClientDisconnected
	ClientMessage
)

type ClientEvent struct {
	Kind     ClientEventKind
	ClientID string
	Outbound chan<- oscmsg.Envelope
	Message  oscmsg.Message
}

func (h *Hub) handleClientEvent(ev ClientEvent) {
	switch ev.Kind {
	case ClientConnected:
		h.clients[ev.ClientID] = ev.Outbound
		h.log.Printf("client connected: %s (total %d)", ev.ClientID, len(h.clients))
		h.sendTo(ev.ClientID, ev.Outbound, oscmsg.NewStatusEnvelope(string(h.status), h.locked))
		h.sendTo(ev.ClientID, ev.Outbound, oscmsg.NewConfigEnvelope(h.buildConfig()))

	case ClientDisconnected:
		delete(h.clients, ev.ClientID)
		delete(h.dropStreak, ev.ClientID)
		h.log.Printf("client disconnected: %s (total %d)", ev.ClientID, len(h.clients))

	case ClientMessage:
		h.handleClientMessage(ev.ClientID, ev.Message)
	}
}

func (h *Hub) handleClientMessage(clientID string, msg oscmsg.Message) {
	if h.debug {
		h.debugf("client-path %s from %s", msg.Address, clientID)
	}

	if h.status == StatusStarting || h.status == StatusReloading || h.status == StatusDegraded {
		if h.debug {
			h.debugf("  status=%s: dropping client message", h.status)
		}
		return
	}

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

const maxConsecutiveDrops = 64

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

func (h *Hub) broadcast(msg oscmsg.Message, source broadcastSource) {
	fromDesk := source.clientID == "" && h.isDeskSource(source.ip)
	if h.debug {
		h.debugf("broadcast %s: source.ip=%q source.clientID=%q deskIP=%q fromDesk=%v externalDevices=%d",
			msg.Address, source.ip, source.clientID, h.deskIP, fromDesk, len(h.externalDevices))
	}
	if !fromDesk {
		h.lastClientDriven[msg.Address] = time.Now()
		if err := h.desk.Send(msg); err != nil {
			h.logSendErrThrottled("desk", "broadcast to desk: %v", err)
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
		if !ext.Loopback && source.ip != "" && ext.Addr.IP.String() == source.ip {
			if h.debug {
				h.debugf("  -> external %q: skipped (loopback source)", ext.Name)
			}
			continue
		}
		if err := h.desk.SendTo(msg, ext.Addr); err != nil {
			h.logSendErrThrottled(ext.Addr.String(), "broadcast to external %q: %v", ext.Name, err)
		} else if h.debug {
			h.debugf("  -> external %q at %s: sent", ext.Name, ext.Addr)
		}
	}

	if fromDesk {
		if last, ok := h.lastClientDriven[msg.Address]; ok && time.Since(last) < deskEchoSuppressWindow {
			if h.debug {
				h.debugf("  -> clients: skipped (desk echo of a recently client-driven %s)", msg.Address)
			}
			return
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

func (h *Hub) logSendErrThrottled(key, format string, args ...any) {
	now := time.Now()
	if last, ok := h.lastSendErrLogged[key]; ok && now.Sub(last) < sendErrLogInterval {
		return
	}
	h.lastSendErrLogged[key] = now
	h.log.Printf(format, args...)
}
