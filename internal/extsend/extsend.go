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
			s.setName(d.Name)
			continue
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

func (m *Manager) Close() {
	stopped := make([]*sender, 0, len(m.senders))
	for k, s := range m.senders {
		s.stop()
		stopped = append(stopped, s)
		delete(m.senders, k)
	}
	for _, s := range stopped {
		<-s.exited
	}
}

type sender struct {
	addr string
	name atomic.Pointer[string]
	log  *log.Logger

	dial         func(*net.UDPAddr) (writer, error)
	udpAddr      *net.UDPAddr
	writeTimeout time.Duration
	backoff      time.Duration

	queue  chan []byte
	done   chan struct{}
	exited chan struct{}

	down    atomic.Bool
	dropped atomic.Uint64

	// Owned by the sender goroutine.
	w            writer
	deadline     time.Time
	lastLogged   time.Time
	windowStart  time.Time
	reportedDown bool
}

func (m *Manager) newSender(d Device) *sender {
	s := &sender{
		addr:         d.Addr.String(),
		log:          m.log,
		dial:         m.dial,
		udpAddr:      d.Addr,
		writeTimeout: m.writeTimeout,
		backoff:      m.backoff,
		queue:        make(chan []byte, m.queueSize),
		done:         make(chan struct{}),
		exited:       make(chan struct{}),
		windowStart:  time.Now(),
	}
	s.setName(d.Name)
	return s
}

func (s *sender) setName(name string) { s.name.Store(&name) }

func (s *sender) label() string { return fmt.Sprintf("external %q (%s)", *s.name.Load(), s.addr) }

func (s *sender) enqueue(pkt []byte) bool {
	if s.down.Load() {
		s.dropped.Add(1)
		return false
	}
	select {
	case s.queue <- pkt:
		return true
	default:
		s.dropped.Add(1)
		return false
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
			s.dropped.Add(1)
			return s.backOff(now, fmt.Errorf("dial: %w", err))
		}
		s.w = w
		s.deadline = time.Time{}
	}

	// re-arm only when less than half is left, not for every packet of a burst
	if s.deadline.Sub(now) < s.writeTimeout/2 {
		s.deadline = now.Add(s.writeTimeout)
		if err := s.w.SetWriteDeadline(s.deadline); err != nil {
			s.dropped.Add(1)
			s.logThrottled(now, fmt.Sprintf("set write deadline: %v", err))
			return true
		}
	}

	_, err := s.w.Write(pkt)
	if err == nil {
		if s.reportedDown {
			s.reportedDown = false
			s.log.Printf("%s: reachable again", s.label())
		}
		if s.dropped.Load() != 0 {
			s.logThrottled(now, "queue full")
		}
		return true
	}

	s.dropped.Add(1)
	if isUnreachable(err) {
		return s.backOff(now, err)
	}
	s.logThrottled(now, fmt.Sprintf("send error: %v", err))
	return true
}

func isUnreachable(err error) bool {
	return errors.Is(err, os.ErrDeadlineExceeded) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.EHOSTDOWN) ||
		errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.ECONNREFUSED)
}

func (s *sender) backOff(now time.Time, cause error) bool {
	s.down.Store(true)
	for drained := false; !drained; {
		select {
		case <-s.queue:
			s.dropped.Add(1)
		default:
			drained = true
		}
	}

	s.reportedDown = s.logThrottled(now, fmt.Sprintf("unreachable (%v)", cause)) || s.reportedDown

	t := time.NewTimer(s.backoff)
	defer t.Stop()
	select {
	case <-s.done:
		return false
	case <-t.C:
	}
	s.down.Store(false)
	return true
}

func (s *sender) logThrottled(now time.Time, what string) bool {
	if !s.lastLogged.IsZero() && now.Sub(s.lastLogged) < logInterval {
		return false
	}
	n := s.dropped.Swap(0)
	window := now.Sub(s.windowStart).Round(time.Second)
	s.lastLogged, s.windowStart = now, now
	s.log.Printf("%s: %s, dropped %d msg(s) in last %s", s.label(), what, n, window)
	return true
}
