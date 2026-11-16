package hub

import (
	"fmt"
	"io"
	"log"
	"net"
	"testing"

	"github.com/mederham/oscwebmixer-go/internal/extsend"
	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
)

type noopDesk struct{}

func (noopDesk) Send(oscmsg.Message) error   { return nil }
func (noopDesk) SendEncoded([]byte) error    { return nil }
func (noopDesk) SetRemote(string, int) error { return nil }
func (noopDesk) Rebind(int) error            { return nil }

type discardExternal struct{}

func (discardExternal) Sync([]extsend.Device)             {}
func (discardExternal) Enqueue(*net.UDPAddr, []byte) bool { return true }
func (discardExternal) Seen(*net.UDPAddr)                 {}
func (discardExternal) SetDebug(bool)                     {}

func benchHub(b *testing.B, clients int) *Hub {
	b.Helper()
	h := New(noopDesk{}, log.New(io.Discard, "", 0), Options{DeskIP: "10.0.0.18"})
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

func BenchmarkBroadcast(b *testing.B) {
	for _, n := range []int{1, 3, 8} {
		b.Run(fmt.Sprintf("devices=%d", n), func(b *testing.B) {
			h := benchHub(b, 2)
			h.external = discardExternal{}
			for i := 0; i < n; i++ {
				h.externalDevices = append(h.externalDevices, ExternalDevice{
					Name:      fmt.Sprintf("dev%d", i),
					Addr:      &net.UDPAddr{IP: net.IPv4(10, 0, 0, byte(50+i)), Port: 9000},
					Broadcast: true,
				})
			}
			msg := oscmsg.Message{Address: "/Input_Channels/24/Aux_Send/3/send_level", Args: []any{float32(-12.5)}}
			src := deskSource("10.0.0.18")
			b.ReportAllocs()
			for b.Loop() {
				h.broadcast(msg, src)
			}
		})
	}
}
