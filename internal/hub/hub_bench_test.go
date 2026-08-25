package hub

import (
	"io"
	"log"
	"net"
	"testing"

	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
)

// noopDesk answers nothing — the point of these benchmarks is per-message
// hub-side cost (cache lookup, broadcast fan-out), not desk round-trip time.
type noopDesk struct{}

func (noopDesk) Send(oscmsg.Message) error                    { return nil }
func (noopDesk) SendTo(oscmsg.Message, *net.UDPAddr) error { return nil }

// benchHub builds an already-Healthy hub (bypassing the startup sequence
// entirely) with n connected clients whose outbound channels are drained by
// a background goroutine, so sendTo's non-blocking select never has to
// choose the drop path mid-benchmark.
func benchHub(b *testing.B, clients int) *Hub {
	b.Helper()
	h := New(noopDesk{}, nil, log.New(io.Discard, "", 0), Options{DeskIP: "10.0.0.18"})
	h.status = StatusHealthy
	h.channelCount, h.auxCount, h.groupCount = 48, 8, 4

	for i := 0; i < clients; i++ {
		out := make(chan oscmsg.Envelope, 4096)
		h.clients[string(rune('a'+i))] = out
		go func(c <-chan oscmsg.Envelope) {
			for range c {
			}
		}(out)
	}
	return h
}

// BenchmarkHandleDeskMessage_Cacheable measures the common case: a message
// whose address matches one of maybeCache's whitelist patterns (an
// Aux_Send level update — the highest-volume real traffic per the plan's
// meter-flood observations).
func BenchmarkHandleDeskMessage_Cacheable(b *testing.B) {
	h := benchHub(b, 2)
	msg := oscmsg.Message{Address: "/Input_Channels/24/Aux_Send/3/send_level", Args: []any{float32(-12.5)}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.handleDeskMessage(msg, "10.0.0.18")
	}
}

// BenchmarkHandleDeskMessage_NonCacheable measures the worst case for
// maybeCache's linear regex scan: an address that matches NONE of the 11
// patterns, so every single one runs to completion before giving up. Meter
// data (/Meters/...) and plenty of other real desk addresses fall here.
func BenchmarkHandleDeskMessage_NonCacheable(b *testing.B) {
	h := benchHub(b, 2)
	msg := oscmsg.Message{Address: "/Meters/1/Input_Channels/24/rms", Args: []any{float32(-40)}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.handleDeskMessage(msg, "10.0.0.18")
	}
}

// BenchmarkMaybeCache_NonCacheable isolates just the regex scan itself,
// stripped of broadcast/logging overhead.
func BenchmarkMaybeCache_NonCacheable(b *testing.B) {
	h := benchHub(b, 0)
	msg := oscmsg.Message{Address: "/Meters/1/Input_Channels/24/rms", Args: []any{float32(-40)}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.maybeCache(msg)
	}
}
