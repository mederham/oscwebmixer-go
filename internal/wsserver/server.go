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

const outboundBufferSize = 256

var nextClientID atomic.Int64

type Server struct {
	upgrader websocket.Upgrader
	events   chan<- hub.ClientEvent
	log      *log.Logger
}

func New(events chan<- hub.ClientEvent, logger *log.Logger) *Server {
	return &Server{
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			CheckOrigin:     func(r *http.Request) bool { return true },
		},
		events: events,
		log:    logger,
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.log.Printf("ws upgrade: %v", err)
		return
	}

	id := fmt.Sprintf("client-%d", nextClientID.Add(1))
	outbound := make(chan oscmsg.Envelope, outboundBufferSize)

	s.events <- hub.ClientEvent{Kind: hub.ClientConnected, ClientID: id, Outbound: outbound}
	defer func() {
		s.events <- hub.ClientEvent{Kind: hub.ClientDisconnected, ClientID: id}
	}()

	done := make(chan struct{})
	go s.writePump(conn, outbound, done)
	s.readPump(conn, id, done)
}

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
			continue
		}

		s.events <- hub.ClientEvent{Kind: hub.ClientMessage, ClientID: id, Message: *env.Message}
	}
}

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
