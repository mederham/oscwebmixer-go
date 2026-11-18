package extsend

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	QueueSize    = 1024
	WriteTimeout = 5 * time.Millisecond
	Backoff      = 3 * time.Second
	logInterval  = time.Minute
)

type Device struct {
	Name string
	Addr *net.UDPAddr
}

type writer interface {
	Write([]byte) (int, error)
	SetWriteDeadline(time.Time) error
	Close() error
}

func dialUDP(addr *net.UDPAddr) (writer, error) {
	sock, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return nil, err
	}
	return sock, nil
}

// Manager is not safe for concurrent use: Sync, Enqueue and Close must be
// called from the same goroutine (the hub's), so the map needs no lock.
type Manager struct {
	log *log.Logger

	dial         func(*net.UDPAddr) (writer, error)
	queueSize    int
	writeTimeout time.Duration
	backoff      time.Duration

	debug atomic.Bool

	senders map[netip.AddrPort]*sender
}

func New(logger *log.Logger) *Manager {
	return &Manager{
		log:          logger,
		dial:         dialUDP,
		queueSize:    QueueSize,
		writeTimeout: WriteTimeout,
		backoff:      Backoff,
		senders:      make(map[netip.AddrPort]*sender),
	}
}

func (m *Manager) SetDebug(on bool) { m.debug.Store(on) }

func key(addr *net.UDPAddr) netip.AddrPort {
	ap := addr.AddrPort()
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}

func (m *Manager) Sync(devices []Device) {
	want := make(map[netip.AddrPort]Device, len(devices))
	for _, d := range devices {
		if d.Addr == nil {
			continue
		}
		k := key(d.Addr)
		if _, dup := want[k]; !dup {
			want[k] = d
		}
	}

	for k, s := range m.senders {
		if _, ok := want[k]; !ok {
			s.stop()
			delete(m.senders, k)
		}
	}

	for k, d := range want {
		if s, ok := m.senders[k]; ok {
			if s.name == d.Name {
				continue
			}
			s.stop()
		}
		s := m.newSender(d)
		m.senders[k] = s
		go s.run()
	}
}

func (m *Manager) Enqueue(addr *net.UDPAddr, pkt []byte) bool {
	s, ok := m.senders[key(addr)]
	if !ok {
		return false
	}
	return s.enqueue(pkt)
}

func (m *Manager) Seen(addr *net.UDPAddr) {
	if s, ok := m.senders[key(addr)]; ok {
		s.seen()
	}
}

func (m *Manager) Close() {
	for _, s := range m.senders {
		s.stop()
	}
	for _, s := range m.senders {
		<-s.exited
	}
	clear(m.senders)
}

type sender struct {
	name  string
	log   *log.Logger
	debug *atomic.Bool

	dial         func(*net.UDPAddr) (writer, error)
	udpAddr      *net.UDPAddr
	writeTimeout time.Duration
	backoff      time.Duration

	queue  chan []byte
	wake   chan struct{}
	done   chan struct{}
	exited chan struct{}

	down  atomic.Bool
	alive atomic.Bool

	droppedBackoff atomic.Uint64
	droppedFull    atomic.Uint64
	droppedFailed  atomic.Uint64

	// Owned by the sender goroutine.
	w            writer
	deadline     time.Time
	lastWarned   time.Time
	lastSummary  time.Time
	windowStart  time.Time
	lastFailed   time.Time
	reportedDown bool
}

func (m *Manager) newSender(d Device) *sender {
	s := &sender{
		name:         d.Name,
		log:          m.log,
		debug:        &m.debug,
		dial:         m.dial,
		udpAddr:      d.Addr,
		writeTimeout: m.writeTimeout,
		backoff:      m.backoff,
		queue:        make(chan []byte, m.queueSize),
		wake:         make(chan struct{}, 1),
		done:         make(chan struct{}),
		exited:       make(chan struct{}),
		windowStart:  time.Now(),
	}
	return s
}

func (s *sender) label() string {
	return fmt.Sprintf("external device %q (%s)", s.name, s.udpAddr)
}

func (s *sender) enqueue(pkt []byte) bool {
	if s.down.Load() {
		s.droppedBackoff.Add(1)
		return false
	}
	select {
	case s.queue <- pkt:
		return true
	default:
		s.droppedFull.Add(1)
		return false
	}
}

func (s *sender) seen() {
	s.alive.Store(true)
	if s.down.Load() {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

func (s *sender) stop() { close(s.done) }

func (s *sender) run() {
	defer close(s.exited)
	defer func() {
		if s.w != nil {
			s.w.Close()
		}
	}()

	for {
		select {
		case <-s.done:
			return
		case pkt := <-s.queue:
			if !s.send(pkt) {
				return
			}
		}
	}
}

func (s *sender) send(pkt []byte) bool {
	now := time.Now()

	if s.w == nil {
		w, err := s.dial(s.udpAddr)
		if err != nil {
			s.droppedFailed.Add(1)
			return s.backOff(now, fmt.Errorf("dial: %w", err))
		}
		s.w = w
		s.deadline = time.Time{}
	}

	// re-arm only when less than half is left, not for every packet of a burst
	if s.deadline.Sub(now) < s.writeTimeout/2 {
		s.deadline = now.Add(s.writeTimeout)
		if err := s.w.SetWriteDeadline(s.deadline); err != nil {
			s.droppedFailed.Add(1)
			s.warn(now, "send error: "+shortErr(err), fmt.Sprintf("set write deadline (%v)", err))
			return true
		}
	}

	_, err := s.w.Write(pkt)
	if err == nil {
		if s.reportedDown && s.alive.Load() && now.Sub(s.lastFailed) >= s.backoff {
			s.reportedDown = false
			s.log.Printf("%s reachable again", s.label())
		}
		if s.droppedFull.Load() != 0 {
			s.warn(now, "too slow, dropping messages", "queue full")
		}
		return true
	}

	s.droppedFailed.Add(1)
	if isUnreachable(err) {
		return s.backOff(now, err)
	}
	s.warn(now, "send error: "+shortErr(err), fmt.Sprintf("send error (%v)", err))
	return true
}

func shortErr(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno.Error()
	}
	return err.Error()
}

func isUnreachable(err error) bool {
	return errors.Is(err, os.ErrDeadlineExceeded) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.EHOSTDOWN) ||
		errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.ECONNREFUSED)
}

func (s *sender) backOff(now time.Time, cause error) bool {
	s.lastFailed = now
	s.down.Store(true)
	s.alive.Store(false)
	select {
	case <-s.wake:
	default:
	}
	for drained := false; !drained; {
		select {
		case <-s.queue:
			s.droppedBackoff.Add(1)
		default:
			drained = true
		}
	}

	if !s.reportedDown {
		s.reportedDown = true
		s.log.Printf("%s not reachable", s.label())
	}
	s.debugSummary(now, fmt.Sprintf("unreachable (%v)", cause))

	t := time.NewTimer(s.backoff)
	defer t.Stop()
	select {
	case <-s.done:
		return false
	case <-s.wake:
		s.down.Store(false)
		return true
	case <-t.C:
		s.down.Store(false)
		return true
	}
}

func (s *sender) warn(now time.Time, msg, detail string) {
	if s.lastWarned.IsZero() || now.Sub(s.lastWarned) >= logInterval {
		s.lastWarned = now
		s.log.Printf("%s %s", s.label(), msg)
	}
	s.debugSummary(now, detail)
}

func (s *sender) debugSummary(now time.Time, what string) {
	if !s.debug.Load() {
		return
	}
	if !s.lastSummary.IsZero() && now.Sub(s.lastSummary) < logInterval {
		return
	}
	backedOff, full, failed := s.droppedBackoff.Swap(0), s.droppedFull.Swap(0), s.droppedFailed.Swap(0)
	window := now.Sub(s.windowStart).Round(time.Second)
	s.lastSummary, s.windowStart = now, now
	s.log.Printf("[debug] %s: %s, dropped %d msg(s) in last %s (backed off %d, queue full %d, send failed %d)",
		s.label(), what, backedOff+full+failed, window, backedOff, full, failed)
}
