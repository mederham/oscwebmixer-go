package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/url"
	"time"

	"github.com/gorilla/websocket"

	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
)

func main() {
	addr := flag.String("addr", "localhost:8080", "host:port of the running oscwebmixer HTTP server")
	channel := flag.Int("channel", 1, "channel number to move")
	aux := flag.Int("aux", 1, "aux bus to send to")
	levelDb := flag.Float64("level-db", -20, "level in dB to set")
	flag.Parse()

	u := url.URL{Scheme: "ws", Host: *addr, Path: "/ws"}
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	for i := 0; i < 2; i++ {
		var env oscmsg.Envelope
		if err := conn.ReadJSON(&env); err != nil {
			log.Fatalf("read: %v", err)
		}
		data, _ := json.Marshal(env)
		fmt.Printf("<- %s\n", data)
	}

	address := fmt.Sprintf("/Input_Channels/%d/Aux_Send/%d/send_level", *channel, *aux)
	out := oscmsg.NewOSCEnvelope(oscmsg.Message{Address: address, Args: []any{float32(*levelDb)}})
	if err := conn.WriteJSON(out); err != nil {
		log.Fatalf("write: %v", err)
	}
	fmt.Printf("-> sent %s = %v\n", address, *levelDb)

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		var env oscmsg.Envelope
		if err := conn.ReadJSON(&env); err != nil {
			break
		}
		data, _ := json.Marshal(env)
		fmt.Printf("<- %s\n", data)
	}
}
