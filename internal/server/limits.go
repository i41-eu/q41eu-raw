package server

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Limits for a server on the internet (raw.q41.eu): failed robot logins and wrong pairing
// codes per client address, and how much an invited (guest) robot may send.
const (
	failWindow        = 10 * time.Minute
	maxRobotAuthFails = 20 // failed robot logins per address and robot id per window, then 429
	maxPairFails      = 20 // wrong pairing codes per address per window, then 429
	maxTrackedAddrs   = 10000

	// A robot streams camera (5 fps JPEG, ~10–20 KB each), microphone (~48 KB/s), the IMU
	// at 100 Hz and telemetry; uploads of pictures come the other way. The budget covers
	// that with room to spare; a guest above it is disconnected.
	guestBytesPerSec = 512 << 10
	guestBurstBytes  = 4 << 20
	guestMsgsPerSec  = 300
	guestBurstMsgs   = 1000
)

// failLimiter counts failures per key (a client address) in a sliding window. Disabled,
// it never blocks: when every client shows up with the same proxy address, a per-address
// limit would lock everybody out at once.
type failLimiter struct {
	max      int
	window   time.Duration
	disabled bool

	mu    sync.Mutex
	fails map[string][]time.Time
}

func newFailLimiter(max int, window time.Duration, disabled bool) *failLimiter {
	return &failLimiter{max: max, window: window, disabled: disabled, fails: map[string][]time.Time{}}
}

// blocked reports whether addr has used up its failures in the window.
func (l *failLimiter) blocked(addr string, now time.Time) bool {
	if l.disabled {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent(addr, now)) >= l.max
}

// fail records one failure for addr.
func (l *failLimiter) fail(addr string, now time.Time) {
	if l.disabled {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.fails) >= maxTrackedAddrs {
		for a := range l.fails {
			if len(l.recent(a, now)) == 0 {
				delete(l.fails, a)
			}
		}
		if len(l.fails) >= maxTrackedAddrs {
			l.fails = map[string][]time.Time{} // under a wide attack: forget, rather than grow
		}
	}
	l.fails[addr] = append(l.recent(addr, now), now)
}

// recent prunes and returns addr's failures inside the window. Must hold l.mu.
func (l *failLimiter) recent(addr string, now time.Time) []time.Time {
	ts := l.fails[addr]
	i := 0
	for i < len(ts) && now.Sub(ts[i]) >= l.window {
		i++
	}
	ts = ts[i:]
	if len(ts) == 0 {
		delete(l.fails, addr)
		return nil
	}
	l.fails[addr] = ts
	return ts
}

// bucket is a token bucket: rate per second, up to burst.
type bucket struct {
	rate, burst float64
	tokens      float64
	last        time.Time
}

func newBucket(rate, burst float64, now time.Time) *bucket {
	return &bucket{rate: rate, burst: burst, tokens: burst, last: now}
}

// take spends n tokens if there are enough.
func (b *bucket) take(n float64, now time.Time) bool {
	b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	b.last = now
	if b.tokens < n {
		return false
	}
	b.tokens -= n
	return true
}

// guestLimits is what one invited robot may send.
type guestLimits struct {
	bytes, msgs *bucket
}

func newGuestLimits(now time.Time) *guestLimits {
	return &guestLimits{
		bytes: newBucket(guestBytesPerSec, guestBurstBytes, now),
		msgs:  newBucket(guestMsgsPerSec, guestBurstMsgs, now),
	}
}

// allow spends one message of size n; false means the robot is over its limits.
func (g *guestLimits) allow(n int, now time.Time) bool {
	return g.msgs.take(1, now) && g.bytes.take(float64(n), now)
}

// clientIP is the address limits and logs use. Behind cfg.TrustedProxies reverse proxies
// that append to X-Forwarded-For (Envoy does), it is the entry the outermost trusted proxy
// added; the entries before it come from the client and can be forged. With no trusted
// proxy, X-Forwarded-For is ignored.
func (s *Server) clientIP(r *http.Request) string {
	if n := s.cfg.TrustedProxies; n > 0 {
		var hops []string
		for _, v := range r.Header.Values("X-Forwarded-For") {
			for _, p := range strings.Split(v, ",") {
				if p = strings.TrimSpace(p); p != "" {
					hops = append(hops, p)
				}
			}
		}
		if len(hops) >= n {
			return hops[len(hops)-n]
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// Browser limits: open event streams and media sockets per session and per address, and a
// command budget per session, so one page (or a script) cannot tie up the server or flood a
// robot.
const (
	maxSSEPerSession = 8
	maxSSEPerAddr    = 30
	maxMediaPerAddr  = 20
	// Sensor streams (IMU, touch, light) per session and robot: apart from the tier's media limit.
	maxSensorsPerRobot = 6
	// The close code of a media socket refused for too many streams; its reason says which.
	closeTooMany      = 4429
	maxCommandBuckets = 10000
	// Per session, by tier (tiers.go): commands per second and burst, media sockets per robot.
)

// streamLimiter counts open streams per key.
type streamLimiter struct {
	mu   sync.Mutex
	open map[string]int
}

func newStreamLimiter() *streamLimiter { return &streamLimiter{open: map[string]int{}} }

// acquire opens one stream for every key, or none if any key is at its limit.
func (l *streamLimiter) acquire(keys map[string]int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, max := range keys {
		if l.open[k] >= max {
			return false
		}
	}
	for k := range keys {
		l.open[k]++
	}
	return true
}

func (l *streamLimiter) release(keys map[string]int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k := range keys {
		if l.open[k]--; l.open[k] <= 0 {
			delete(l.open, k)
		}
	}
}

// streamKeys are the limits for one stream: always per session, per address unless the
// server cannot see client addresses.
func (s *Server) streamKeys(kind, session, ip string, perSession, perAddr int) map[string]int {
	keys := map[string]int{kind + " session " + session: perSession}
	if !s.cfg.NoAddressLimits {
		keys[kind+" addr "+ip] = perAddr
	}
	return keys
}

// commandAllowed spends one command from the session's budget (its tier's rate).
func (s *Server) commandAllowed(session string, now time.Time) bool {
	tl := tierTable[s.sessionTier(session, now)]
	key := fmt.Sprintf("%s|%v", session, tl.CommandsPerSec) // a new budget after signing in
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()
	b := s.cmdBuckets[key]
	if b == nil {
		if len(s.cmdBuckets) >= maxCommandBuckets {
			for k, v := range s.cmdBuckets {
				if now.Sub(v.last) > time.Minute {
					delete(s.cmdBuckets, k)
				}
			}
			if len(s.cmdBuckets) >= maxCommandBuckets {
				s.cmdBuckets = map[string]*bucket{}
			}
		}
		b = newBucket(tl.CommandsPerSec, tl.CommandBurst, now)
		s.cmdBuckets[key] = b
	}
	return b.take(1, now)
}
