package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mederham/oscwebmixer-go/internal/desk"
	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
)

func main() {
	host := flag.String("host", "127.0.0.1", "IP oscwebmixer's OSC listener is bound on")
	port := flag.Int("port", 8000, "oscwebmixer's OSC.Port (config/global.json's osc.port)")
	channels := flag.Int("channels", 48, "number of channels to sweep (1..N)")
	aux := flag.Int("aux", 1, "aux bus number (Aux_Send/N)")
	rateHz := flag.Float64("rate", 30, "updates per second, per channel")
	waveHz := flag.Float64("wave-hz", 0.2, "wave sweep frequency (cycles/sec)")
	minDB := flag.Float64("min-db", -60, "wave trough level (dB)")
	maxDB := flag.Float64("max-db", 0, "wave crest level (dB)")
	duration := flag.Duration("duration", 0, "stop after this long (0 = run until interrupted)")
	flag.Parse()

	conn, err := desk.Listen(0, *host, *port)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	defer conn.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	addresses := make([]string, *channels)
	for i := range addresses {
		addresses[i] = fmt.Sprintf("/Input_Channels/%d/Aux_Send/%d/send_level", i+1, *aux)
	}
	phaseStep := 2 * math.Pi / float64(*channels)

	tick := time.NewTicker(time.Duration(float64(time.Second) / *rateHz))
	defer tick.Stop()
	report := time.NewTicker(2 * time.Second)
	defer report.Stop()

	start := time.Now()
	var sent, errs int64

	log.Printf("sending to %s:%d — %d channels, aux %d, %.1f updates/sec/channel (%.0f msg/sec total), wave %.2f Hz",
		*host, *port, *channels, *aux, *rateHz, *rateHz*float64(*channels), *waveHz)

	for {
		select {
		case <-ctx.Done():
			log.Printf("done: sent %d message(s), %d error(s), in %s", sent, errs, time.Since(start))
			return
		case <-report.C:
			log.Printf("sent %d message(s) so far (%d error(s))", sent, errs)
		case now := <-tick.C:
			t := now.Sub(start).Seconds()
			for i, addr := range addresses {
				phase := t*2*math.Pi*(*waveHz) + float64(i)*phaseStep
				level := float32(*minDB + (*maxDB-*minDB)*(0.5+0.5*math.Sin(phase)))
				if err := conn.Send(oscmsg.Message{Address: addr, Args: []any{level}}); err != nil {
					errs++
					continue
				}
				sent++
			}
		}
	}
}
