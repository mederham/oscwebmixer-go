// Package wsserver handles WS upgrade and the per-client read/write pump
// pattern gorilla/websocket requires (its connection isn't safe for
// concurrent writes, so exactly one goroutine — the write pump — ever
// calls WriteMessage). It registers/deregisters with hub via ClientEvent
// and translates JSON frames <-> oscmsg.Envelope. Accepts every upgrade
// unconditionally — no "loaded" gate at accept time (see plan: healthcheck
// redesign; that gate is what caused the original bug).
package wsserver

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync/atomic"

	"github.com/gorilla/websocket"

	"github.com/mederham/oscwebmixer-go/internal/hub"
	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
)

// outboundBufferSize is generous but bounded: a wedged/backgrounded
// client's channel filling up must never block the hub goroutine (see
// plan: "Backpressure policy"). hub already never blocks on a full
// channel; this package is where a chronically-full client would
// eventually get disconnected — not yet implemented (see Serve's doc
// comment), just the bounded, non-blocking piece hub already relies on.
const outboundBufferSize = 256

var nextClientID atomic.Int64

func newClientID() string {
	return fmt.Sprintf("client-%d", nextClientID.Add(1))
}

// Server upgrades HTTP connections to WebSocket and bridges them to hub.
type Server struct {
	upgrader websocket.Upgrader
	events   chan<- hub.ClientEvent
	log      *log.Logger
}

// New builds a Server. events is hub's client-event channel (the second
// channel passed to Hub.Run).
func New(events chan<- hub.ClientEvent, logger *log.Logger) *Server {
	return &Server{
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			// No origin restriction: this serves a LAN device with no
			// account/auth model, same trust boundary as the JS version.
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		events: events,
		log:    logger,
	}
}

// ServeHTTP upgrades the connection and blocks for the lifetime of the WS
// session, registering/deregistering with hub around it.
//
// KNOWN GAP: a client whose outbound channel is chronically full (per the
// plan's backpressure policy, this should get it disconnected) is not yet
// detected/dropped here — hub's sendTo just drops individual messages and
// logs. Revisit if this proves to matter in practice; not needed for the
// Phase 2 demo.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.log.Printf("ws upgrade: %v", err)
		return
	}

	id := newClientID()
	outbound := make(chan oscmsg.Envelope, outboundBufferSize)

	s.events <- hub.ClientEvent{Kind: hub.ClientConnected, ClientID: id, Outbound: outbound}
	defer func() {
		s.events <- hub.ClientEvent{Kind: hub.ClientDisconnected, ClientID: id}
	}()

	done := make(chan struct{})
	go s.writePump(conn, outbound, done)
	s.readPump(conn, id, done)
}

// readPump is the only goroutine allowed to call conn.ReadMessage.
func (s *Server) readPump(conn *websocket.Conn, id string, done chan struct{}) {
	defer close(done)
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}

		var env oscmsg.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			s.log.Printf("client %s: malformed envelope: %v", id, err)
			continue
		}
		if env.Kind != oscmsg.KindOSC || env.Message == nil {
			// Clients only ever originate osc-kind messages — status/config
			// are server-to-client only.
			continue
		}

		s.events <- hub.ClientEvent{Kind: hub.ClientMessage, ClientID: id, Message: *env.Message}
	}
}

// writePump is the only goroutine allowed to call conn.WriteMessage — the
// required pattern for gorilla/websocket, whose connection is not safe for
// concurrent writes.
func (s *Server) writePump(conn *websocket.Conn, outbound <-chan oscmsg.Envelope, done <-chan struct{}) {
	defer conn.Close()
	for {
		select {
		case env, ok := <-outbound:
			if !ok {
				return
			}
			data, err := json.Marshal(env)
			if err != nil {
				continue
			}
			if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}
		case <-done:
			return
		}
	}
}
