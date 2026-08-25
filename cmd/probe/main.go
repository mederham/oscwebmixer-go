// probe is the Phase 0 throwaway CLI for validating github.com/mederham/osc
// against the real desk: send one OSC query, print everything that comes
// back within a timeout window (a reply's address often differs from the
// query's — e.g. "/Console/Channels/?" answers as "/Console/Input_Channels"
// — so probe does not filter for an exact address match). -listen instead
// sends nothing and prints whatever arrives, unprompted, until Ctrl+C —
// for watching what the desk sends on its own initiative (e.g. does it
// really send /Console/Session/! on a session change, per the plan's
// Phase 4 assumption) rather than in reply to a query.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mederham/oscwebmixer-go/internal/desk"
	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
)

func main() {
	deskIP := flag.String("desk-ip", "10.0.0.18", "desk IP address")
	deskPort := flag.Int("desk-port", 9000, "port the desk receives on")
	localPort := flag.Int("listen-port", 8000, "local port the desk sends replies to")
	address := flag.String("address", "/Console/Channels/?", "OSC address to query (ignored with -listen)")
	timeout := flag.Duration("timeout", 3*time.Second, "how long to wait for replies (ignored with -listen)")
	listenOnly := flag.Bool("listen", false, "send nothing, just print whatever arrives until Ctrl+C")
	flag.Parse()

	conn, err := desk.Listen(*localPort, *deskIP, *deskPort)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	defer conn.Close()

	go func() {
		// Run exits with an error once Close() is called on shutdown (via
		// the deferred conn.Close() above); that's expected at process
		// exit, not a failure worth reporting for this throwaway probe.
		_ = conn.Run(context.Background())
	}()

	var deadline <-chan time.Time
	if *listenOnly {
		fmt.Printf("listening on :%d for anything from %s:%d — Ctrl+C to stop\n", *localPort, *deskIP, *deskPort)
	} else {
		fmt.Printf("sending %s to %s:%d, listening on :%d\n", *address, *deskIP, *deskPort, *localPort)
		if err := conn.Send(oscmsg.Message{Address: *address, Args: []any{}}); err != nil {
			log.Fatalf("send: %v", err)
		}
		deadline = time.After(*timeout)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	count := 0
	for {
		select {
		case in := <-conn.Inbound():
			count++
			fmt.Printf("[%d] %s from %s: %s %s\n", count, time.Now().Format("15:04:05.000"), in.From, in.Message.Address, formatArgs(in.Message.Args))
		case <-deadline:
			if count == 0 {
				fmt.Println("no reply received within timeout — check desk IP/port and network reachability")
				os.Exit(1)
			}
			fmt.Printf("done: %d message(s) received\n", count)
			return
		case <-ctx.Done():
			fmt.Printf("\nstopped: %d message(s) received\n", count)
			return
		}
	}
}

// formatArgs renders each argument with its Go type so int32 vs float32
// (indistinguishable via fmt's default %v for whole-number floats) is
// visible — the whole point of this probe is nailing down that distinction
// per address.
func formatArgs(args []any) string {
	s := "["
	for i, a := range args {
		if i > 0 {
			s += " "
		}
		s += fmt.Sprintf("%v(%T)", a, a)
	}
	return s + "]"
}
