// Package desk is the UDP transport to the DiGiCo desk. It owns the socket
// only — no cache, no broadcast, no business logic (those live in
// internal/hub, per the plan).
package desk

import (
	"context"
	"fmt"
	"log"
	"net"

	"github.com/mederham/osc"

	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
)

// recvBufferBytes raises the OS UDP receive buffer above its default so a
// burst of desk traffic has headroom to sit in the kernel while Run is
// momentarily busy handing off the previous packet — the second line of
// defense against the dropped-packet issue from the plan (the first is Run
// never blocking on anything but the channel send below).
const recvBufferBytes = 4 << 20 // 4 MiB

// inboundBufferSize is deliberately generous: hub falling behind under load
// must never cause Run to block on ReadFromUDP.
const inboundBufferSize = 4096

// InboundMessage pairs a decoded message with who actually sent it — a
// packet on this socket isn't necessarily from the desk, since configured
// external OSC devices can share the same local port (mirrors the current
// JS design).
type InboundMessage struct {
	Message oscmsg.Message
	From    *net.UDPAddr
}

// Conn is the UDP transport to the desk.
type Conn struct {
	sock  *net.UDPConn
	raddr *net.UDPAddr

	inbound chan InboundMessage
}

// Listen opens the local UDP socket the desk (and any configured external
// OSC devices) send to, and remembers the desk's address as the default
// send target for Send.
func Listen(localPort int, deskIP string, deskPort int) (*Conn, error) {
	ip := net.ParseIP(deskIP)
	if ip == nil {
		return nil, fmt.Errorf("invalid desk ip %q", deskIP)
	}

	sock, err := net.ListenUDP("udp", &net.UDPAddr{Port: localPort})
	if err != nil {
		return nil, fmt.Errorf("listen udp :%d: %w", localPort, err)
	}
	if err := sock.SetReadBuffer(recvBufferBytes); err != nil {
		// Not fatal — some platforms/permissions restrict this — but worth
		// surfacing since it's the fallback line of defense against
		// dropped packets under load.
		log.Printf("desk: warning: could not raise UDP read buffer: %v", err)
	}

	return &Conn{
		sock:    sock,
		raddr:   &net.UDPAddr{IP: ip, Port: deskPort},
		inbound: make(chan InboundMessage, inboundBufferSize),
	}, nil
}

// Inbound returns the channel of decoded messages read from the socket.
func (c *Conn) Inbound() <-chan InboundMessage { return c.inbound }

// Run reads raw bytes off the socket, decodes, and hands off — nothing
// else. All further processing (cache, broadcast, plugin pipeline) happens
// exclusively in the hub, never here, so this loop is never the thing
// standing between the kernel's receive buffer and going back for the next
// packet. Run blocks until ctx is canceled or the socket errors.
func (c *Conn) Run(ctx context.Context) error {
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			c.sock.Close()
		case <-stop:
		}
	}()

	buf := make([]byte, 65536)
	for {
		n, addr, err := c.sock.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("read udp: %w", err)
		}

		raw := make([]byte, n)
		copy(raw, buf[:n])

		oscMsg, err := osc.ParseMessage(raw, addr)
		if err != nil {
			// Malformed packet — drop and keep reading; one bad packet
			// must never stall the loop. Logged (not just silently
			// dropped) since a sender whose packets never parse would
			// otherwise look identical to a sender that never sends at
			// all — see the real desk's own "Invalid OSC message" for
			// /Layout/Layout/Banks' TB In row, confirmed live.
			log.Printf("desk: malformed packet from %s: %v", addr, err)
			continue
		}
		msg, err := fromOSC(oscMsg)
		if err != nil {
			log.Printf("desk: undecodable message %q from %s: %v", oscMsg.Address, addr, err)
			continue
		}

		select {
		case c.inbound <- InboundMessage{Message: msg, From: addr}:
		default:
			// Inbound channel is full: hub has fallen far behind the
			// generous buffer. Drop rather than block the reader — a full
			// channel here signals the buffer size needs revisiting, not
			// something to paper over by blocking.
			log.Printf("desk: inbound channel full, dropping %s from %s", msg.Address, addr)
		}
	}
}

// Send sends to the desk's configured address.
func (c *Conn) Send(msg oscmsg.Message) error {
	return c.SendTo(msg, c.raddr)
}

// SendTo sends to an arbitrary address (used for configured external OSC
// devices, not just the desk).
func (c *Conn) SendTo(msg oscmsg.Message, addr *net.UDPAddr) error {
	oscMsg, err := toOSC(msg)
	if err != nil {
		return err
	}
	_, err = c.sock.WriteToUDP(oscMsg.Bytes(), addr)
	return err
}

// Close closes the underlying socket, which also unblocks Run.
func (c *Conn) Close() error {
	return c.sock.Close()
}
