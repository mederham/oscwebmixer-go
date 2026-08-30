package desk

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/mederham/osc"

	"github.com/mederham/oscwebmixer-go/internal/oscmsg"
)

func sendUDP(t *testing.T, port int, payload []byte) {
	t.Helper()
	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
	if err != nil {
		t.Fatalf("dial udp: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write udp: %v", err)
	}
}

// assertReceivesPing reads exactly one packet off sock and confirms it's
// the /ping message pingPayload (below) encodes.
func assertReceivesPing(t *testing.T, sock *net.UDPConn) {
	t.Helper()
	sock.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 1024)
	n, addr, err := sock.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	oscMsg, err := osc.ParseMessage(buf[:n], addr)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if oscMsg.Address != "/ping" {
		t.Fatalf("address = %q, want /ping", oscMsg.Address)
	}
}

func pingPayload(t *testing.T) []byte {
	t.Helper()
	oscMsg, err := toOSC(oscmsg.Message{Address: "/ping", Args: []any{}})
	if err != nil {
		t.Fatalf("toOSC: %v", err)
	}
	return oscMsg.Bytes()
}

// TestRebindContinuesReceivingOnNewPort proves Rebind lets the same Run
// goroutine — and the same Inbound() channel a caller may already be
// reading from — pick up traffic on a new local port in place, rather
// than requiring the reader goroutine to be torn down and restarted (see
// Run's doc comment for how a deliberate Rebind close is told apart from
// a real socket failure).
func TestRebindContinuesReceivingOnNewPort(t *testing.T) {
	// Fixed, not ephemeral: the test needs to send packets *to* these
	// ports from outside the Conn, and Conn has no way to report back
	// which port net.ListenUDP(...Port:0) would have picked.
	const oldPort, newPort = 29191, 29192

	c, err := Listen(oldPort, "127.0.0.1", 29999)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- c.Run(ctx) }()

	payload := pingPayload(t)

	sendUDP(t, oldPort, payload)
	select {
	case in := <-c.Inbound():
		if in.Message.Address != "/ping" {
			t.Fatalf("got %q, want /ping", in.Message.Address)
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive on the original port before rebinding")
	}

	if err := c.Rebind(newPort); err != nil {
		t.Fatalf("Rebind: %v", err)
	}

	sendUDP(t, newPort, payload)
	select {
	case in := <-c.Inbound():
		if in.Message.Address != "/ping" {
			t.Fatalf("got %q, want /ping", in.Message.Address)
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive on the new port after rebinding — Run did not pick up the rebound socket")
	}

	cancel()
	select {
	case err := <-runErr:
		if err != context.Canceled {
			t.Fatalf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

// TestSetRemoteChangesSendTarget proves SetRemote redirects Send without
// touching the local socket at all — see SetRemote's doc comment for why
// this needs no rebind, unlike the local-port case above.
func TestSetRemoteChangesSendTarget(t *testing.T) {
	recv1, err := net.ListenUDP("udp", &net.UDPAddr{Port: 0})
	if err != nil {
		t.Fatalf("listen recv1: %v", err)
	}
	defer recv1.Close()
	recv2, err := net.ListenUDP("udp", &net.UDPAddr{Port: 0})
	if err != nil {
		t.Fatalf("listen recv2: %v", err)
	}
	defer recv2.Close()

	recv1Port := recv1.LocalAddr().(*net.UDPAddr).Port
	recv2Port := recv2.LocalAddr().(*net.UDPAddr).Port

	c, err := Listen(0, "127.0.0.1", recv1Port)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer c.Close()

	msg := oscmsg.Message{Address: "/ping", Args: []any{}}
	if err := c.Send(msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	assertReceivesPing(t, recv1)

	if err := c.SetRemote("127.0.0.1", recv2Port); err != nil {
		t.Fatalf("SetRemote: %v", err)
	}
	if err := c.Send(msg); err != nil {
		t.Fatalf("Send after SetRemote: %v", err)
	}
	assertReceivesPing(t, recv2)
}

func TestListenWithoutDeskIPThenSetRemote(t *testing.T) {
	recv, err := net.ListenUDP("udp", &net.UDPAddr{Port: 0})
	if err != nil {
		t.Fatalf("listen recv: %v", err)
	}
	defer recv.Close()
	recvPort := recv.LocalAddr().(*net.UDPAddr).Port

	c, err := Listen(0, "", 0)
	if err != nil {
		t.Fatalf("Listen with empty desk IP: %v", err)
	}
	defer c.Close()

	msg := oscmsg.Message{Address: "/ping", Args: []any{}}
	if err := c.Send(msg); err == nil {
		t.Fatal("Send with no desk configured returned nil error, want an error")
	}

	if err := c.SetRemote("127.0.0.1", recvPort); err != nil {
		t.Fatalf("SetRemote: %v", err)
	}
	if err := c.Send(msg); err != nil {
		t.Fatalf("Send after SetRemote: %v", err)
	}
	assertReceivesPing(t, recv)
}

func TestSetRemoteRejectsInvalidIP(t *testing.T) {
	c, err := Listen(0, "127.0.0.1", 1)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer c.Close()

	if err := c.SetRemote("not-an-ip", 9000); err == nil {
		t.Fatal("SetRemote with an invalid IP returned nil error, want an error")
	}
}
