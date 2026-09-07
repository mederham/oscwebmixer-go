package hub

import (
	"io"
	"log"
	"net"
	"testing"

	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
)

type noopDesk struct{}

func (noopDesk) Send(oscmsg.Message) error                 { return nil }
func (noopDesk) SendTo(oscmsg.Message, *net.UDPAddr) error { return nil }
func (noopDesk) SetRemote(string, int) error               { return nil }
func (noopDesk) Rebind(int) error                          { return nil }

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

func BenchmarkHandleDeskMessage_Cacheable(b *testing.B) {
	h := benchHub(b, 2)
	msg := oscmsg.Message{Address: "/Input_Channels/24/Aux_Send/3/send_level", Args: []any{float32(-12.5)}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.handleDeskMessage(msg, "10.0.0.18")
	}
}

func BenchmarkHandleDeskMessage_NonCacheable(b *testing.B) {
	h := benchHub(b, 2)
	msg := oscmsg.Message{Address: "/Meters/1/Input_Channels/24/rms", Args: []any{float32(-40)}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.handleDeskMessage(msg, "10.0.0.18")
	}
}

func BenchmarkMaybeCache_NonCacheable(b *testing.B) {
	h := benchHub(b, 0)
	msg := oscmsg.Message{Address: "/Meters/1/Input_Channels/24/rms", Args: []any{float32(-40)}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.maybeCache(msg)
	}
}
