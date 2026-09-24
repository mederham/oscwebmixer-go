package hub

import (
	"fmt"
	"time"

	"github.com/mederham/oscwebmixer-go/internal/config"
	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
)

type AdminEventKind int

const (
	AdminQuery AdminEventKind = iota
	AdminApply
	AdminStatusQuery
	AdminNetworkApply
	AdminSetLocked
	AdminSendAuxOn
)

type DeskStatusInfo struct {
	Status   string
	LastSeen time.Time
	Locked   bool
}

type AdminEvent struct {
	Kind        AdminEventKind
	Reply       chan<- oscmsg.ConfigPayload
	StatusReply chan<- DeskStatusInfo

	Layouts                 map[string]config.NamedLayout
	Auxes                   []config.Aux
	ExternalDevices         []ExternalDevice
	AutoHideChannelPatterns []string
	AutoHideGroupPatterns   []string
	AutoHideAuxPatterns     []string
	Debug                   bool

	DeskIP   string
	DeskPort int
	OSCPort  int

	Locked bool

	Messages []oscmsg.Message
}

func sourceKey(sourceType string, number int) string {
	return fmt.Sprintf("%s:%d", sourceType, number)
}

func (h *Hub) handleAdminEvent(ev AdminEvent) {
	switch ev.Kind {
	case AdminQuery:
		select {
		case ev.Reply <- h.buildConfig():
		default:
		}

	case AdminApply:
		h.layouts = ev.Layouts
		h.auxOverrides = ev.Auxes
		h.externalDevices = ev.ExternalDevices
		h.autoHideChannelPatterns = ev.AutoHideChannelPatterns
		h.autoHideGroupPatterns = ev.AutoHideGroupPatterns
		h.autoHideAuxPatterns = ev.AutoHideAuxPatterns
		h.debug = ev.Debug

		h.log.Printf("admin config applied: %d layout(s), %d aux override(s), %d external device(s)",
			len(ev.Layouts), len(ev.Auxes), len(ev.ExternalDevices))

		cfg := oscmsg.NewConfigEnvelope(h.buildConfig())
		for id, out := range h.clients {
			h.sendTo(id, out, cfg)
		}

	case AdminStatusQuery:
		select {
		case ev.StatusReply <- DeskStatusInfo{Status: string(h.status), LastSeen: h.lastDeskSeen, Locked: h.locked}:
		default:
		}

	case AdminNetworkApply:
		h.applyNetworkChange(ev)

	case AdminSetLocked:
		h.locked = ev.Locked
		h.log.Printf("admin: personal mixing locked = %v", h.locked)
		h.pushStatus()

	case AdminSendAuxOn:
		for _, msg := range ev.Messages {
			h.broadcast(msg, adminSource())
		}
	}
}

func (h *Hub) applyNetworkChange(ev AdminEvent) {
	if ev.DeskIP != h.deskIP || ev.DeskPort != h.deskPort {
		if err := h.desk.SetRemote(ev.DeskIP, ev.DeskPort); err != nil {
			h.log.Printf("admin apply: set desk address: %v", err)
		} else {
			h.deskIP, h.deskPort = ev.DeskIP, ev.DeskPort
			h.log.Printf("admin apply: desk address now %s:%d", ev.DeskIP, ev.DeskPort)
		}
	}
	if ev.OSCPort != h.oscPort {
		if err := h.desk.Rebind(ev.OSCPort); err != nil {
			h.log.Printf("admin apply: rebind OSC listen port: %v", err)
		} else {
			h.oscPort = ev.OSCPort
			h.log.Printf("admin apply: OSC listen port now %d", ev.OSCPort)
		}
	}
}
