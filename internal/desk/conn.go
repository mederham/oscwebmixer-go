// Package desk is the UDP transport to the DiGiCo desk. It owns the socket
// only — no cache, no broadcast, no business logic (those live in
// internal/hub, per the plan).
package desk

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"

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

// Conn is the UDP transport to the desk. sock/raddr are mutable after
// construction — SetRemote/Rebind let the admin panel apply a changed
// Desk IP/port or OSC listen port live, without restarting the process —
// so both are guarded by mu since Run/Send/SendTo read them from the hub
// goroutine concurrently with an admin apply.
type Conn struct {
	mu    sync.Mutex
	sock  *net.UDPConn
	raddr *net.UDPAddr

	// sendSocks holds one dedicated, connected outbound socket per
	// destination address (keyed by addr.String()), instead of writing
	// every destination through the shared receive socket above. A
	// per-destination socket isolates OS-level "destination unreachable"
	// errors (ICMP host/net unreachable) to the one address that caused
	// them — on a shared unconnected socket, the kernel latches that error
	// onto the socket itself and returns it on the *next* send call
	// regardless of target, silently failing sends to a completely
	// different, healthy destination (confirmed live: enabling one
	// offline external device dropped almost all desk traffic too).
	// Entries are never evicted — an address that stops being used (e.g.
	// after SetRemote or an admin device-IP edit) just leaves one idle
	// socket cached for the life of the process, which is negligible.
	sendSocks map[string]*net.UDPConn

	inbound chan InboundMessage
}

// Listen opens the local UDP socket the desk (and any configured external
// OSC devices) send to, and remembers the desk's address as the default
// send target for Send. deskIP may be empty — the router-container config
// is meant to come up with no desk set and get pointed at one later via the
// admin panel's Network tab (SetRemote), rather than requiring it at
// process start; Send returns an error in the meantime rather than sending
// anywhere.
func Listen(localPort int, deskIP string, deskPort int) (*Conn, error) {
	var raddr *net.UDPAddr
	if deskIP != "" {
		ip := net.ParseIP(deskIP)
		if ip == nil {
			return nil, fmt.Errorf("invalid desk ip %q", deskIP)
		}
		raddr = &net.UDPAddr{IP: ip, Port: deskPort}
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
		sock:      sock,
		raddr:     raddr,
		sendSocks: make(map[string]*net.UDPConn),
		inbound:   make(chan InboundMessage, inboundBufferSize),
	}, nil
}

// Inbound returns the channel of decoded messages read from the socket.
func (c *Conn) Inbound() <-chan InboundMessage { return c.inbound }

// Run reads raw bytes off the socket, decodes, and hands off — nothing
// else. All further processing (cache, broadcast, plugin pipeline) happens
// exclusively in the hub, never here, so this loop is never the thing
// standing between the kernel's receive buffer and going back for the next
// packet. Run blocks until ctx is canceled or the socket errors.
//
// Reads the current socket fresh every iteration (under mu) rather than
// once outside the loop, so a Rebind mid-flight is picked up without
// restarting this goroutine or losing the caller's Inbound() channel
// reference — see the error handling below for how a Rebind's deliberate
// close is told apart from a real socket failure.
func (c *Conn) Run(ctx context.Context) error {
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			c.mu.Lock()
			c.sock.Close()
			c.mu.Unlock()
		case <-stop:
		}
	}()

	buf := make([]byte, 65536)
	for {
		c.mu.Lock()
		sock := c.sock
		c.mu.Unlock()

		n, addr, err := sock.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			c.mu.Lock()
			reboundAway := c.sock != sock
			c.mu.Unlock()
			if reboundAway {
				// This socket was deliberately replaced by Rebind, not a
				// real failure — loop back and read from the new one.
				continue
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

// Send sends to the desk's configured address. Errors if no desk has been
// configured yet (see Listen's doc comment) — there's nowhere to send to.
func (c *Conn) Send(msg oscmsg.Message) error {
	c.mu.Lock()
	raddr := c.raddr
	c.mu.Unlock()
	if raddr == nil {
		return fmt.Errorf("desk: no desk configured")
	}
	return c.SendTo(msg, raddr)
}

// SendTo sends to an arbitrary address (used for configured external OSC
// devices, not just the desk). Each destination gets its own dedicated
// outbound socket — see sendSocks' doc comment for why — rather than going
// out through the shared receive socket.
func (c *Conn) SendTo(msg oscmsg.Message, addr *net.UDPAddr) error {
	oscMsg, err := toOSC(msg)
	if err != nil {
		return err
	}
	sock, err := c.sendSock(addr)
	if err != nil {
		return err
	}
	_, err = sock.Write(oscMsg.Bytes())
	return err
}

// sendSock returns the dedicated outbound socket for addr, creating and
// caching it on first use. The source port is whatever the OS assigns —
// devices reply to a separately configured fixed address, not to whatever
// port a request happened to come from, so there's no need for outbound
// sends to share the receive socket's local port.
func (c *Conn) sendSock(addr *net.UDPAddr) (*net.UDPConn, error) {
	key := addr.String()

	c.mu.Lock()
	sock, ok := c.sendSocks[key]
	c.mu.Unlock()
	if ok {
		return sock, nil
	}

	sock, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return nil, fmt.Errorf("dial udp %s: %w", addr, err)
	}

	c.mu.Lock()
	if existing, ok := c.sendSocks[key]; ok {
		c.mu.Unlock()
		sock.Close()
		return existing, nil
	}
	c.sendSocks[key] = sock
	c.mu.Unlock()
	return sock, nil
}

// SetRemote changes the desk's address that Send targets — no socket
// rebind needed, since raddr is just the default destination for an
// otherwise-unconnected UDP socket (see Listen), not a "connection" of any
// kind. Lets the admin panel apply a changed Desk IP/port live.
func (c *Conn) SetRemote(ip string, port int) error {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return fmt.Errorf("invalid desk ip %q", ip)
	}
	c.mu.Lock()
	c.raddr = &net.UDPAddr{IP: parsed, Port: port}
	c.mu.Unlock()
	return nil
}

// Rebind closes the current socket and opens a new one on localPort,
// letting Run's read loop pick up the new socket in place — see Run's doc
// comment. Lets the admin panel apply a changed OSC listen port live,
// unlike Desk IP/port above, this really does need a new socket since the
// old one is bound to the previous local port.
func (c *Conn) Rebind(localPort int) error {
	sock, err := net.ListenUDP("udp", &net.UDPAddr{Port: localPort})
	if err != nil {
		return fmt.Errorf("listen udp :%d: %w", localPort, err)
	}
	if err := sock.SetReadBuffer(recvBufferBytes); err != nil {
		log.Printf("desk: warning: could not raise UDP read buffer: %v", err)
	}

	c.mu.Lock()
	old := c.sock
	c.sock = sock
	c.mu.Unlock()

	old.Close() // unblocks Run's ReadFromUDP on the old socket
	return nil
}

// Close closes the underlying socket, which also unblocks Run, plus every
// dedicated outbound socket opened by SendTo.
func (c *Conn) Close() error {
	c.mu.Lock()
	sock := c.sock
	sendSocks := c.sendSocks
	c.sendSocks = make(map[string]*net.UDPConn)
	c.mu.Unlock()
	for _, s := range sendSocks {
		s.Close()
	}
	return sock.Close()
}
