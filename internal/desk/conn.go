package desk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/mederham/osc"

	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
)

var ErrNoDesk = errors.New("desk: no desk configured")

var ErrDeskUnreachable = errors.New("desk: unreachable, backing off")

const recvBufferBytes = 4 << 20

const inboundBufferSize = 4096

// Without a deadline, a write to an unanswered IP on the local subnet blocks
// the hub for seconds once the socket's send buffer fills during ARP resolution.
const deskWriteTimeout = 20 * time.Millisecond

// After a timeout, sends fail fast for this long instead of each waiting out the deadline.
const deskBackoff = time.Second

type InboundMessage struct {
	Message oscmsg.Message
	From    *net.UDPAddr
}

type Conn struct {
	mu    sync.Mutex
	sock  *net.UDPConn
	raddr *net.UDPAddr

	downUntil time.Time

	inbound chan InboundMessage
}

func Listen(localPort int, deskIP string, deskPort int) (*Conn, error) {
	var raddr *net.UDPAddr
	if deskIP != "" {
		ip := net.ParseIP(deskIP)
		if ip == nil {
			return nil, fmt.Errorf("invalid desk ip %q", deskIP)
		}
		raddr = &net.UDPAddr{IP: ip, Port: deskPort}
	}

	sock, err := newListenSocket(localPort)
	if err != nil {
		return nil, err
	}

	return &Conn{
		sock:    sock,
		raddr:   raddr,
		inbound: make(chan InboundMessage, inboundBufferSize),
	}, nil
}

func (c *Conn) Inbound() <-chan InboundMessage { return c.inbound }

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
				continue
			}
			return fmt.Errorf("read udp: %w", err)
		}

		raw := bytes.Clone(buf[:n])

		oscMsg, err := osc.ParseMessage(raw, addr)
		if err != nil {
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
			log.Printf("desk: inbound channel full, dropping %s from %s", msg.Address, addr)
		}
	}
}

func (c *Conn) Send(msg oscmsg.Message) error {
	pkt, err := Encode(msg)
	if err != nil {
		return err
	}
	return c.SendEncoded(pkt)
}

func (c *Conn) SendEncoded(pkt []byte) error {
	now := time.Now()
	c.mu.Lock()
	raddr, sock, downUntil := c.raddr, c.sock, c.downUntil
	c.mu.Unlock()
	if raddr == nil {
		return ErrNoDesk
	}
	if now.Before(downUntil) {
		return ErrDeskUnreachable
	}
	if err := sock.SetWriteDeadline(now.Add(deskWriteTimeout)); err != nil {
		return err
	}
	_, err := sock.WriteToUDP(pkt, raddr)
	c.noteWriteErr(err)
	return err
}

func (c *Conn) noteWriteErr(err error) {
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		return
	}
	c.mu.Lock()
	c.downUntil = time.Now().Add(deskBackoff)
	c.mu.Unlock()
}

func (c *Conn) SetRemote(ip string, port int) error {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return fmt.Errorf("invalid desk ip %q", ip)
	}
	c.mu.Lock()
	c.raddr = &net.UDPAddr{IP: parsed, Port: port}
	c.downUntil = time.Time{}
	c.mu.Unlock()
	return nil
}

func (c *Conn) Rebind(localPort int) error {
	sock, err := newListenSocket(localPort)
	if err != nil {
		return err
	}

	c.mu.Lock()
	old := c.sock
	c.sock = sock
	c.mu.Unlock()

	old.Close()
	return nil
}

func newListenSocket(localPort int) (*net.UDPConn, error) {
	sock, err := net.ListenUDP("udp", &net.UDPAddr{Port: localPort})
	if err != nil {
		return nil, fmt.Errorf("listen udp :%d: %w", localPort, err)
	}
	if err := sock.SetReadBuffer(recvBufferBytes); err != nil {
		log.Printf("desk: warning: could not raise UDP read buffer: %v", err)
	}
	return sock, nil
}

func (c *Conn) Close() error {
	c.mu.Lock()
	sock := c.sock
	c.mu.Unlock()
	return sock.Close()
}
