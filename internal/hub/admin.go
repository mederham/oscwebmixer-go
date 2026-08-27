package hub

import (
	"fmt"
	"time"

	"github.com/mederham/oscwebmixer-go/internal/config"
	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
)

// AdminEventKind distinguishes the things internal/admin needs from hub:
// reading the current state to render the admin UI (hub state is only
// safe to read from Run's own goroutine — see the plan's "Concurrency
// model" — so this goes through the same request/response-over-channel
// pattern as everything else), applying a freshly-saved config, and
// reading desk connectivity status for the Network tab's indicator.
type AdminEventKind int

const (
	AdminQuery AdminEventKind = iota
	AdminApply
	AdminStatusQuery
)

// DeskStatusInfo answers AdminStatusQuery — the Network tab's status dot
// and "last seen" line. Kept separate from AdminQuery's ConfigPayload
// deliberately: ConfigPayload is also the WS wire type pushed to every
// mixer client, and desk status has no reason to ride along on that; this
// is a distinct, admin-only, on-demand read.
type DeskStatusInfo struct {
	Status   string
	LastSeen time.Time
}

// AdminEvent is the "admin config change" member of the tagged union the
// plan's "Concurrency model" section describes hub's Run loop processing.
// internal/admin owns writing to disk; only the already-persisted result
// (or a query for the current one) ever crosses into hub.
type AdminEvent struct {
	Kind        AdminEventKind
	Reply       chan<- oscmsg.ConfigPayload // set for AdminQuery
	StatusReply chan<- DeskStatusInfo       // set for AdminStatusQuery

	// The following are set for AdminApply.
	Layout          []config.LayoutItem
	Auxes           []config.Aux
	ExternalDevices []ExternalDevice
}

// sourceKey identifies a source by its stable identity (type+number) —
// not by array/list position, since admin drag-and-drop/reorder means
// position isn't identity (see plan: "Group buses").
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
		h.layout = ev.Layout
		h.auxOverrides = ev.Auxes
		h.externalDevices = ev.ExternalDevices

		h.log.Printf("admin config applied: %d layout item(s), %d aux override(s), %d external device(s)",
			len(ev.Layout), len(ev.Auxes), len(ev.ExternalDevices))

		// Live, no restart, no disconnect — every connected client gets a
		// fresh config push (see the "config on WS, push-based" decision;
		// same mechanism used on connect).
		cfg := oscmsg.NewConfigEnvelope(h.buildConfig())
		for _, out := range h.clients {
			h.sendTo(out, cfg)
		}

	case AdminStatusQuery:
		select {
		case ev.StatusReply <- DeskStatusInfo{Status: string(h.status), LastSeen: h.lastDeskSeen}:
		default:
		}
	}
}
