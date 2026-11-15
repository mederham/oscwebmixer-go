package desk

import (
	"context"
	"errors"
	"net"
	"os"
	"syscall"
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

func TestRebindContinuesReceivingOnNewPort(t *testing.T) {
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

func TestSendRefreshesWriteDeadline(t *testing.T) {
	recv, err := net.ListenUDP("udp", &net.UDPAddr{Port: 0})
	if err != nil {
		t.Fatalf("listen recv: %v", err)
	}
	defer recv.Close()

	c, err := Listen(0, "127.0.0.1", recv.LocalAddr().(*net.UDPAddr).Port)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer c.Close()

	msg := oscmsg.Message{Address: "/ping", Args: []any{}}
	if err := c.Send(msg); err != nil {
		t.Fatalf("first Send: %v", err)
	}
	assertReceivesPing(t, recv)

	time.Sleep(2 * deskWriteTimeout)
	if err := c.Send(msg); err != nil {
		t.Fatalf("Send after the first deadline passed: %v", err)
	}
	assertReceivesPing(t, recv)
}

func TestSendFailsFastWhileBackedOff(t *testing.T) {
	recv, err := net.ListenUDP("udp", &net.UDPAddr{Port: 0})
	if err != nil {
		t.Fatalf("listen recv: %v", err)
	}
	defer recv.Close()
	recvPort := recv.LocalAddr().(*net.UDPAddr).Port

	c, err := Listen(0, "127.0.0.1", recvPort)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer c.Close()

	c.downUntil = time.Now().Add(time.Hour)
	msg := oscmsg.Message{Address: "/ping", Args: []any{}}
	if err := c.Send(msg); !errors.Is(err, ErrDeskUnreachable) {
		t.Fatalf("Send while backed off = %v, want ErrDeskUnreachable", err)
	}
	recv.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, _, err := recv.ReadFromUDP(make([]byte, 64)); err == nil {
		t.Fatal("desk received a packet while backed off")
	}

	if err := c.SetRemote("127.0.0.1", recvPort); err != nil {
		t.Fatalf("SetRemote: %v", err)
	}
	if err := c.Send(msg); err != nil {
		t.Fatalf("Send after SetRemote: %v", err)
	}
	assertReceivesPing(t, recv)
}

func TestOnlyWriteTimeoutStartsBackoff(t *testing.T) {
	c, err := Listen(0, "127.0.0.1", 1)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer c.Close()

	c.noteWriteErr(nil)
	c.noteWriteErr(&net.OpError{Op: "write", Net: "udp", Err: os.NewSyscallError("sendto", syscall.ECONNREFUSED)})
	if !c.downUntil.IsZero() {
		t.Fatalf("backoff started on a non-timeout error (downUntil = %v)", c.downUntil)
	}

	c.noteWriteErr(&net.OpError{Op: "write", Net: "udp", Err: os.ErrDeadlineExceeded})
	if !c.downUntil.After(time.Now()) {
		t.Fatal("write timeout didn't start a backoff")
	}
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
