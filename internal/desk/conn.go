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

const recvBufferBytes = 4 << 20

const inboundBufferSize = 4096

type InboundMessage struct {
	Message oscmsg.Message
	From    *net.UDPAddr
}

type Conn struct {
	mu    sync.Mutex
	sock  *net.UDPConn
	raddr *net.UDPAddr

	sendSocks map[string]*net.UDPConn

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

	sock, err := net.ListenUDP("udp", &net.UDPAddr{Port: localPort})
	if err != nil {
		return nil, fmt.Errorf("listen udp :%d: %w", localPort, err)
	}
	if err := sock.SetReadBuffer(recvBufferBytes); err != nil {
		log.Printf("desk: warning: could not raise UDP read buffer: %v", err)
	}

	return &Conn{
		sock:      sock,
		raddr:     raddr,
		sendSocks: make(map[string]*net.UDPConn),
		inbound:   make(chan InboundMessage, inboundBufferSize),
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

		raw := make([]byte, n)
		copy(raw, buf[:n])

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
	c.mu.Lock()
	raddr := c.raddr
	sock := c.sock
	c.mu.Unlock()
	if raddr == nil {
		return fmt.Errorf("desk: no desk configured")
	}
	oscMsg, err := toOSC(msg)
	if err != nil {
		return err
	}
	_, err = sock.WriteToUDP(oscMsg.Bytes(), raddr)
	return err
}

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

	old.Close()
	return nil
}

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
