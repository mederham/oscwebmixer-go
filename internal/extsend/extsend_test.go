package extsend

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeWriter ignores deadlines, so block simulates a write stuck in the kernel.
type fakeWriter struct {
	mu        sync.Mutex
	writes    int
	last      []byte
	err       error
	block     chan struct{}
	closed    bool
	deadlines int
}

func (w *fakeWriter) Write(p []byte) (int, error) {
	if w.block != nil {
		<-w.block
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes++
	w.last = p
	if w.err != nil {
		return 0, w.err
	}
	return len(p), nil
}

func (w *fakeWriter) SetWriteDeadline(time.Time) error {
	w.mu.Lock()
	w.deadlines++
	w.mu.Unlock()
	return nil
}

func (w *fakeWriter) Close() error {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
	return nil
}

func (w *fakeWriter) setErr(err error) {
	w.mu.Lock()
	w.err = err
	w.mu.Unlock()
}

func (w *fakeWriter) stats() (writes int, closed bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writes, w.closed
}

type fakeNet struct {
	mu      sync.Mutex
	writers map[string]*fakeWriter
	dials   map[string]int
	dialErr map[string]error // one-shot
}

func newFakeNet() *fakeNet {
	return &fakeNet{writers: map[string]*fakeWriter{}, dials: map[string]int{}, dialErr: map[string]error{}}
}

func (n *fakeNet) writer(addr string) *fakeWriter {
	n.mu.Lock()
	defer n.mu.Unlock()
	w, ok := n.writers[addr]
	if !ok {
		w = &fakeWriter{}
		n.writers[addr] = w
	}
	return w
}

func (n *fakeNet) dial(addr *net.UDPAddr) (writer, error) {
	k := addr.String()
	n.mu.Lock()
	n.dials[k]++
	err := n.dialErr[k]
	delete(n.dialErr, k)
	n.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return n.writer(k), nil
}

func (n *fakeNet) dialCount(addr string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.dials[addr]
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newTestManager(t *testing.T, fn *fakeNet, out io.Writer) *Manager {
	t.Helper()
	m := New(log.New(out, "", 0))
	m.dial = fn.dial
	m.backoff = 200 * time.Millisecond
	t.Cleanup(m.Close)
	return m
}

func udpAddr(ip string, port int) *net.UDPAddr {
	return &net.UDPAddr{IP: net.ParseIP(ip), Port: port}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestEnqueueDeliversOverRealSocket(t *testing.T) {
	recv, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer recv.Close()
	addr := recv.LocalAddr().(*net.UDPAddr)

	m := New(log.New(io.Discard, "", 0))
	defer m.Close()
	m.Sync([]Device{{Name: "local", Addr: addr}})

	if !m.Enqueue(addr, []byte("hello")) {
		t.Fatal("Enqueue returned false for a configured device")
	}
	recv.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 64)
	n, _, err := recv.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf[:n]) != "hello" {
		t.Fatalf("got %q, want hello", buf[:n])
	}
}

func TestEnqueueUnknownAddressDrops(t *testing.T) {
	m := newTestManager(t, newFakeNet(), io.Discard)
	if m.Enqueue(udpAddr("10.0.0.99", 9000), []byte("x")) {
		t.Fatal("Enqueue to an unconfigured address returned true")
	}
}

func TestKeyIgnoresIPv4Representation(t *testing.T) {
	fn := newFakeNet()
	m := newTestManager(t, fn, io.Discard)
	m.Sync([]Device{{Name: "iPad", Addr: udpAddr("10.0.0.12", 9000)}})
	if !m.Enqueue(&net.UDPAddr{IP: net.IPv4(10, 0, 0, 12).To4(), Port: 9000}, []byte("x")) {
		t.Fatal("4-byte IPv4 address didn't match the device configured with net.ParseIP")
	}
}

func TestBlockedDeviceDoesNotBlockOthers(t *testing.T) {
	fn := newFakeNet()
	dead, ok := udpAddr("10.0.0.12", 9000), udpAddr("10.0.0.13", 9000)
	release := make(chan struct{})
	fn.writer(dead.String()).block = release
	good := fn.writer(ok.String())

	m := newTestManager(t, fn, io.Discard)
	m.Sync([]Device{{Name: "dead", Addr: dead}, {Name: "good", Addr: ok}})
	defer close(release) // runs before t.Cleanup's m.Close, which waits for the goroutine

	const rounds, perRound = 10, 500 // perRound < QueueSize so "good" can't overflow
	pkt := []byte("/x")
	var inEnqueue time.Duration
	var deadAccepted int
	for r := 1; r <= rounds; r++ {
		start := time.Now()
		for i := 0; i < perRound; i++ {
			if m.Enqueue(dead, pkt) {
				deadAccepted++
			}
			if !m.Enqueue(ok, pkt) {
				t.Fatalf("round %d msg %d: healthy device dropped a message", r, i)
			}
		}
		inEnqueue += time.Since(start)
		waitFor(t, "healthy device to drain", func() bool {
			n, _ := good.stats()
			return n == r*perRound
		})
	}

	if inEnqueue > 200*time.Millisecond {
		t.Fatalf("spent %s in Enqueue for %d msgs, want < 200ms", inEnqueue, 2*rounds*perRound)
	}
	if deadAccepted > QueueSize+1 {
		t.Fatalf("blocked device accepted %d msgs, want at most %d", deadAccepted, QueueSize+1)
	}
	s := m.senders[key(dead)]
	if got, want := s.droppedFull.Load(), uint64(rounds*perRound-deadAccepted); got != want {
		t.Fatalf("blocked device dropped = %d, want %d", got, want)
	}
}

func TestBackoffSkipsWritesUntilProbe(t *testing.T) {
	cases := map[string]error{
		"host unreachable": &net.OpError{Op: "write", Net: "udp", Err: os.NewSyscallError("write", syscall.EHOSTUNREACH)},
		"net unreachable":  &net.OpError{Op: "write", Net: "udp", Err: os.NewSyscallError("write", syscall.ENETUNREACH)},
		"conn refused":     &net.OpError{Op: "write", Net: "udp", Err: os.NewSyscallError("write", syscall.ECONNREFUSED)},
		"write timeout":    &net.OpError{Op: "write", Net: "udp", Err: os.ErrDeadlineExceeded},
	}
	for name, failure := range cases {
		t.Run(name, func(t *testing.T) {
			fn := newFakeNet()
			addr := udpAddr("10.0.0.12", 9000)
			w := fn.writer(addr.String())
			w.setErr(failure)

			m := newTestManager(t, fn, io.Discard)
			m.Sync([]Device{{Name: "iPad", Addr: addr}})
			s := m.senders[key(addr)]

			start := time.Now()
			m.Enqueue(addr, []byte("first"))
			waitFor(t, "device to be marked down", s.down.Load)

			for time.Since(start) < m.backoff/2 {
				if m.Enqueue(addr, []byte("skipped")) {
					t.Fatal("Enqueue accepted a packet for a backed-off device")
				}
				time.Sleep(time.Millisecond)
			}
			if n, _ := w.stats(); n != 1 {
				t.Fatalf("writes during backoff = %d, want 1 (only the failing one)", n)
			}

			waitFor(t, "backoff to end", func() bool { return !s.down.Load() })
			m.Enqueue(addr, []byte("probe"))
			waitFor(t, "probe to be written", func() bool { n, _ := w.stats(); return n == 2 })
			waitFor(t, "device to be marked down again", s.down.Load)
			m.Enqueue(addr, []byte("skipped"))
			time.Sleep(10 * time.Millisecond)
			if n, _ := w.stats(); n != 2 {
				t.Fatalf("writes after failed probe = %d, want 2", n)
			}

			w.setErr(nil)
			waitFor(t, "second backoff to end", func() bool { return !s.down.Load() })
			for i := 0; i < 5; i++ {
				if !m.Enqueue(addr, []byte("ok")) {
					t.Fatalf("Enqueue %d after recovery returned false", i)
				}
			}
			waitFor(t, "writes after recovery", func() bool { n, _ := w.stats(); return n == 7 })
		})
	}
}

func TestOtherWriteErrorsDoNotBackOff(t *testing.T) {
	fn := newFakeNet()
	addr := udpAddr("10.0.0.12", 9000)
	w := fn.writer(addr.String())
	w.setErr(&net.OpError{Op: "write", Net: "udp", Err: os.NewSyscallError("write", syscall.EACCES)})

	var out syncBuffer
	m := newTestManager(t, fn, &out)
	m.Sync([]Device{{Name: "iPad", Addr: addr}})

	for i := 0; i < 3; i++ {
		if !m.Enqueue(addr, []byte("x")) {
			t.Fatalf("Enqueue %d returned false", i)
		}
	}
	waitFor(t, "all writes attempted", func() bool { n, _ := w.stats(); return n == 3 })
	if m.senders[key(addr)].down.Load() {
		t.Fatal("device backed off on a non-reachability error")
	}
	const want = `external device "iPad" (10.0.0.12:9000) send error: permission denied` + "\n"
	if got := out.String(); got != want {
		t.Fatalf("log = %q, want exactly %q", got, want)
	}
}

func TestDialFailureIsRetriedAfterBackoff(t *testing.T) {
	fn := newFakeNet()
	addr := udpAddr("10.0.0.12", 9000)
	fn.dialErr[addr.String()] = os.NewSyscallError("connect", syscall.ENETUNREACH)

	m := newTestManager(t, fn, io.Discard)
	m.Sync([]Device{{Name: "iPad", Addr: addr}})
	s := m.senders[key(addr)]

	m.Enqueue(addr, []byte("x"))
	waitFor(t, "device to be marked down after dial failure", s.down.Load)
	waitFor(t, "backoff to end", func() bool { return !s.down.Load() })

	m.Enqueue(addr, []byte("y"))
	w := fn.writer(addr.String())
	waitFor(t, "write after re-dial", func() bool { n, _ := w.stats(); return n == 1 })
	if got := fn.dialCount(addr.String()); got != 2 {
		t.Fatalf("dials = %d, want 2", got)
	}
}

func TestSyncLifecycle(t *testing.T) {
	fn := newFakeNet()
	a, b, c := udpAddr("10.0.0.1", 9000), udpAddr("10.0.0.2", 9000), udpAddr("10.0.0.3", 9000)
	m := newTestManager(t, fn, io.Discard)

	m.Sync([]Device{{Name: "A", Addr: a}, {Name: "B", Addr: b}})
	for _, addr := range []*net.UDPAddr{a, b} {
		m.Enqueue(addr, []byte("x"))
		w := fn.writer(addr.String())
		waitFor(t, "first write to "+addr.String(), func() bool { n, _ := w.stats(); return n == 1 })
	}
	senderA, senderB := m.senders[key(a)], m.senders[key(b)]

	m.Sync([]Device{{Name: "A", Addr: a}, {Name: "C", Addr: c}})

	if m.senders[key(a)] != senderA {
		t.Fatal("unchanged device got a new sender")
	}
	if _, ok := m.senders[key(b)]; ok {
		t.Fatal("removed device still has a sender")
	}
	select {
	case <-senderB.exited:
	case <-time.After(time.Second):
		t.Fatal("removed device's goroutine didn't exit")
	}
	if _, closed := fn.writer(b.String()).stats(); !closed {
		t.Fatal("removed device's socket wasn't closed")
	}
	if m.Enqueue(b, []byte("x")) {
		t.Fatal("Enqueue to a removed device returned true")
	}

	m.Enqueue(a, []byte("y"))
	m.Enqueue(c, []byte("y"))
	waitFor(t, "write to A after sync", func() bool { n, _ := fn.writer(a.String()).stats(); return n == 2 })
	waitFor(t, "write to C", func() bool { n, _ := fn.writer(c.String()).stats(); return n == 1 })
	if got := fn.dialCount(a.String()); got != 1 {
		t.Fatalf("A dialled %d times, want 1 (no reconnect on sync)", got)
	}

	m.Sync([]Device{{Name: "A renamed", Addr: a}, {Name: "C", Addr: c}})
	if got := m.senders[key(a)]; got == senderA || got.name != "A renamed" {
		t.Fatal("renamed device kept its old sender")
	}
	select {
	case <-senderA.exited:
	case <-time.After(time.Second):
		t.Fatal("renamed device's old goroutine didn't exit")
	}

	senderA2, senderC := m.senders[key(a)], m.senders[key(c)]
	m.Close()
	for name, s := range map[string]*sender{"A": senderA2, "C": senderC} {
		select {
		case <-s.exited:
		default:
			t.Fatalf("Close returned before %s's goroutine exited", name)
		}
	}
	for _, addr := range []*net.UDPAddr{a, c} {
		if _, closed := fn.writer(addr.String()).stats(); !closed {
			t.Fatalf("socket for %s not closed by Close", addr)
		}
	}
	m.Close()
}

func TestNotReachableIsLoggedOnceUntilRecovery(t *testing.T) {
	fn := newFakeNet()
	addr := udpAddr("10.0.0.12", 8001)
	w := fn.writer(addr.String())
	timeout := &net.OpError{Op: "write", Net: "udp", Err: os.ErrDeadlineExceeded}
	w.setErr(timeout)

	var out syncBuffer
	m := newTestManager(t, fn, &out)
	m.backoff = 100 * time.Millisecond
	m.Sync([]Device{{Name: "iPad 2", Addr: addr}})
	s := m.senders[key(addr)]

	probe := func(i int) {
		t.Helper()
		waitFor(t, "device up for next probe", func() bool { return !s.down.Load() })
		m.Enqueue(addr, []byte("x"))
		waitFor(t, fmt.Sprintf("write %d", i), func() bool { n, _ := w.stats(); return n == i })
	}
	for i := 1; i <= 3; i++ {
		probe(i)
		waitFor(t, "device down after failed probe", s.down.Load)
	}

	const down = `external device "iPad 2" (10.0.0.12:8001) not reachable` + "\n"
	logs := out.String()
	if logs != down {
		t.Fatalf("log after 3 failed probes = %q, want exactly %q", logs, down)
	}

	w.setErr(nil)
	probe(4)
	time.Sleep(10 * time.Millisecond)
	if strings.Contains(out.String(), "reachable again") {
		t.Fatal("logged recovery after a successful UDP write without hearing from the device")
	}

	m.Seen(addr)
	m.Enqueue(addr, []byte("x"))
	waitFor(t, "recovery log", func() bool {
		return strings.Contains(out.String(), `external device "iPad 2" (10.0.0.12:8001) reachable again`)
	})

	w.setErr(timeout)
	probe(6)
	waitFor(t, "second not reachable log", func() bool { return strings.Count(out.String(), "not reachable") == 2 })
}

func TestDebugSummaryIncludesDropDetails(t *testing.T) {
	fn := newFakeNet()
	addr := udpAddr("10.0.0.12", 8001)
	w := fn.writer(addr.String())
	w.setErr(&net.OpError{Op: "write", Net: "udp", Err: os.ErrDeadlineExceeded})

	var out syncBuffer
	m := newTestManager(t, fn, &out)
	m.SetDebug(true)
	m.Sync([]Device{{Name: "iPad 2", Addr: addr}})

	m.Enqueue(addr, []byte("x"))
	waitFor(t, "debug summary", func() bool { return strings.Contains(out.String(), "[debug]") })
	for _, want := range []string{
		`[debug] external device "iPad 2" (10.0.0.12:8001): unreachable (`,
		`(backed off 0, queue full 0, send failed 1)`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("log %q doesn't contain %q", out.String(), want)
		}
	}
}

func TestSeenEndsBackoffImmediately(t *testing.T) {
	fn := newFakeNet()
	addr := udpAddr("10.0.0.12", 8001)
	w := fn.writer(addr.String())
	w.setErr(&net.OpError{Op: "write", Net: "udp", Err: os.NewSyscallError("write", syscall.EHOSTUNREACH)})

	var out syncBuffer
	m := newTestManager(t, fn, &out)
	m.backoff = time.Hour
	m.Sync([]Device{{Name: "iPad", Addr: addr}})
	s := m.senders[key(addr)]

	m.Enqueue(addr, []byte("x"))
	waitFor(t, "device to be marked down", s.down.Load)

	w.setErr(nil)
	m.Seen(addr)
	waitFor(t, "Seen to end the backoff", func() bool { return !s.down.Load() })

	if !m.Enqueue(addr, []byte("y")) {
		t.Fatal("Enqueue after Seen returned false")
	}
	waitFor(t, "write after recovery", func() bool { n, _ := w.stats(); return n == 2 })
	if strings.Contains(out.String(), "reachable again") {
		t.Fatal("logged recovery before a clean backoff period")
	}
}

// Another app on the device's IP (e.g. the stress tool) keeps sending while
// nothing listens on the device's port.
func TestInboundWhileWritesFailDoesNotFlapLog(t *testing.T) {
	fn := newFakeNet()
	addr := udpAddr("10.0.0.12", 8001)
	w := fn.writer(addr.String())
	w.setErr(&net.OpError{Op: "write", Net: "udp", Err: os.NewSyscallError("write", syscall.ECONNREFUSED)})

	var out syncBuffer
	m := newTestManager(t, fn, &out)
	m.backoff = time.Hour
	m.Sync([]Device{{Name: "iPad", Addr: addr}})
	s := m.senders[key(addr)]

	for i := 1; i <= 5; i++ {
		m.Seen(addr)
		waitFor(t, "device up", func() bool { return !s.down.Load() })
		m.Enqueue(addr, []byte("x"))
		waitFor(t, fmt.Sprintf("write %d", i), func() bool { n, _ := w.stats(); return n == i })
		waitFor(t, "device down", s.down.Load)
	}

	const want = `external device "iPad" (10.0.0.12:8001) not reachable` + "\n"
	if got := out.String(); got != want {
		t.Fatalf("log = %q, want exactly %q", got, want)
	}
}

func TestRefusedLocalPortDoesNotAffectOtherDevices(t *testing.T) {
	recv, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer recv.Close()
	good := recv.LocalAddr().(*net.UDPAddr)

	deadSock, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	dead := deadSock.LocalAddr().(*net.UDPAddr)
	deadSock.Close()

	m := New(log.New(io.Discard, "", 0))
	defer m.Close()
	m.Sync([]Device{{Name: "dead", Addr: dead}, {Name: "good", Addr: good}})

	for i := 0; i < 5; i++ {
		m.Enqueue(dead, []byte("x"))
	}
	if !m.Enqueue(good, []byte("hello")) {
		t.Fatal("Enqueue to the healthy device returned false")
	}
	recv.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 64)
	n, _, err := recv.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf[:n]) != "hello" {
		t.Fatalf("got %q, want hello", buf[:n])
	}
}

func BenchmarkEnqueue(b *testing.B) {
	fn := newFakeNet()
	addr := udpAddr("10.0.0.12", 9000)
	m := New(log.New(io.Discard, "", 0))
	m.dial = fn.dial
	defer m.Close()
	m.Sync([]Device{{Name: "iPad", Addr: addr}})
	pkt := []byte("/Input_Channels/24/Aux_Send/3/send_level\x00\x00\x00\x00,f\x00\x00\xc1\x48\x00\x00")
	b.ReportAllocs()
	for b.Loop() {
		m.Enqueue(addr, pkt)
	}
}
