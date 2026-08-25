package hub

import (
	"fmt"

	"github.com/mederham/oscwebmixer-go/internal/config"
	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
)

// AdminEventKind distinguishes the two things internal/admin needs from
// hub: reading the current state to render the admin UI (hub state is only
// safe to read from Run's own goroutine — see the plan's "Concurrency
// model" — so this goes through the same request/response-over-channel
// pattern as everything else), and applying a freshly-saved config.
type AdminEventKind int

const (
	AdminQuery AdminEventKind = iota
	AdminApply
)

// AdminEvent is the "admin config change" member of the tagged union the
// plan's "Concurrency model" section describes hub's Run loop processing.
// internal/admin owns writing to disk; only the already-persisted result
// (or a query for the current one) ever crosses into hub.
type AdminEvent struct {
	Kind  AdminEventKind
	Reply chan<- oscmsg.ConfigPayload // set for AdminQuery

	// The following are set for AdminApply.
	Sources         []config.Source
	Auxes           []config.Aux
	ExternalDevices []ExternalDevice
}

// sourceKey identifies a source by its stable identity (type+number) —
// not by array position, since admin drag-and-drop/reorder means position
// isn't identity (see plan: "Group buses").
func sourceKey(sourceType string, number int) string {
	return fmt.Sprintf("%s:%d", sourceType, number)
}

func indexSources(sources []config.Source) map[string]config.Source {
	m := make(map[string]config.Source, len(sources))
	for _, s := range sources {
		m[sourceKey(s.Type, s.Number)] = s
	}
	return m
}

func (h *Hub) handleAdminEvent(ev AdminEvent) {
	switch ev.Kind {
	case AdminQuery:
		select {
		case ev.Reply <- h.buildConfig():
		default:
		}

	case AdminApply:
		h.sourceOverrides = indexSources(ev.Sources)
		h.auxOverrides = ev.Auxes
		h.externalDevices = ev.ExternalDevices

		h.log.Printf("admin config applied: %d source override(s), %d aux override(s), %d external device(s)",
			len(ev.Sources), len(ev.Auxes), len(ev.ExternalDevices))

		// Live, no restart, no disconnect — every connected client gets a
		// fresh config push (see the "config on WS, push-based" decision;
		// same mechanism used on connect).
		cfg := oscmsg.NewConfigEnvelope(h.buildConfig())
		for _, out := range h.clients {
			h.sendTo(out, cfg)
		}
	}
}
