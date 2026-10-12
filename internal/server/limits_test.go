package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/i41-eu/q41eu-raw/wire"
)

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:4242"
	r.Header.Add("X-Forwarded-For", "203.0.113.7, 198.51.100.2")
	r.Header.Add("X-Forwarded-For", "192.0.2.9")
	for proxies, want := range map[int]string{
		0: "10.0.0.5",     // no trusted proxy: the header is ignored (forgeable)
		1: "192.0.2.9",    // what our one gateway appended
		2: "198.51.100.2", // two proxies
		9: "10.0.0.5",     // fewer hops than proxies: fall back to the peer
	} {
		s := &Server{cfg: Config{TrustedProxies: proxies}}
		if got := s.clientIP(r); got != want {
			t.Errorf("TrustedProxies=%d: got %s, want %s", proxies, got, want)
		}
	}
}

func TestFailLimiter(t *testing.T) {
	l := newFailLimiter(3, time.Minute, false)
	now := time.Now()
	for i := 0; i < 3; i++ {
		if l.blocked("a", now) {
			t.Fatalf("blocked after %d failures", i)
		}
		l.fail("a", now)
	}
	if !l.blocked("a", now) || l.blocked("b", now) {
		t.Fatal("want a blocked, b not")
	}
	if l.blocked("a", now.Add(time.Minute)) {
		t.Fatal("still blocked after the window")
	}
	off := newFailLimiter(1, time.Minute, true)
	off.fail("a", now)
	off.fail("a", now)
	if off.blocked("a", now) {
		t.Fatal("a disabled limiter blocked")
	}
}

func TestBucket(t *testing.T) {
	now := time.Now()
	b := newBucket(10, 20, now)
	if !b.take(20, now) || b.take(1, now) {
		t.Fatal("burst")
	}
	if !b.take(10, now.Add(time.Second)) || b.take(1, now.Add(time.Second)) {
		t.Fatal("refill")
	}
}

func TestRobotLoginsRateLimited(t *testing.T) {
	ts, _ := newTestServer(t)
	for i := 0; i < maxRobotAuthFails; i++ {
		if got := dialStatus(ts, "wrong", "chan-1"); got != http.StatusUnauthorized {
			t.Fatalf("attempt %d: HTTP %d, want 401", i, got)
		}
	}
	// Blocked now for this robot, even with the right token: otherwise guessing would go on.
	if got := dialStatus(ts, testToken, "chan-1"); got != http.StatusTooManyRequests {
		t.Fatalf("after the limit: HTTP %d, want 429", got)
	}
	// Other robots from the same address are not locked out.
	connectRobot(t, ts, "chan-2", wire.ClassRobot)
}

func TestNoAddressLimits(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(Config{RobotToken: testToken, PairTTL: time.Minute, Log: quiet, NoAddressLimits: true})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	for i := 0; i < maxRobotAuthFails+5; i++ {
		dialStatus(ts, "wrong", "chan-1")
	}
	connectRobot(t, ts, "chan-1", wire.ClassRobot)
}

func TestWrongPairCodesRateLimited(t *testing.T) {
	ts, _ := newTestServer(t)
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for i := 0; i < maxPairFails; i++ {
		resp, err := c.Get(ts.URL + "/pair?code=ZZZZZZZZ")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("attempt %d: HTTP %d, want 400", i, resp.StatusCode)
		}
	}
	resp, err := c.Get(ts.URL + "/pair?code=ZZZZZZZZ")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("after the limit: HTTP %d, want 429", resp.StatusCode)
	}
}

func TestGuestRobotFloodDisconnected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "robot-tokens")
	tok, line, _ := NewRobotInvite("guest-1")
	writeTokens(t, path, line)
	ts := newInviteServer(t, path)
	registerGuest(t, ts, tok, "guest-1")
	// The owner's robots are not limited.
	owner := connectRobot(t, ts, "own-1", wire.ClassRobot)

	ws, _, err := dialRobot(ts, tok, "guest-1")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	r := &testRobot{t: t, ws: ws}
	r.send(wire.KindRegister, wire.RegisterBody{Class: wire.ClassRobot,
		Capabilities: wire.RobotCapabilities{Model: "test", Commands: []string{"nod"}}})
	frame := make([]byte, 64<<10) // 64 KB binary messages: 4 MB burst is gone after 64
	frame[0] = wire.BinCameraJPEG
	closed := false
	for i := 0; i < 200 && !closed; i++ {
		if err := ws.WriteMessage(websocket.BinaryMessage, frame); err != nil {
			closed = true
		}
	}
	ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, _, err := ws.ReadMessage()
		if err == nil {
			continue
		}
		ce, ok := err.(*websocket.CloseError)
		if !ok || ce.Code != websocket.ClosePolicyViolation {
			t.Fatalf("want a policy-violation close, got %v (write failed early: %v)", err, closed)
		}
		break
	}
	for i := 0; i < 200; i++ {
		if err := owner.ws.WriteMessage(websocket.BinaryMessage, frame); err != nil {
			t.Fatalf("owner robot cut off at message %d: %v", i, err)
		}
	}
}

func TestEventStreamsPerSession(t *testing.T) {
	ts, _ := newTestServer(t)
	robot := connectRobot(t, ts, "chan-1", wire.ClassRobot)
	browser := pairBrowser(t, robot)
	var open []*http.Response
	for i := 0; i < maxSSEPerSession; i++ {
		resp, err := browser.Get(ts.URL + "/api/events")
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("stream %d: %v %v", i, err, resp)
		}
		open = append(open, resp)
	}
	resp, err := browser.Get(ts.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("stream over the limit: %d", resp.StatusCode)
	}
	// Closing one frees its place.
	open[0].Body.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp, err := browser.Get(ts.URL + "/api/events")
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			break
		}
		if resp != nil {
			resp.Body.Close()
		}
		if time.Now().After(deadline) {
			t.Fatal("a closed stream did not free its place")
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, r := range open[1:] {
		r.Body.Close()
	}
}

func TestCommandBudget(t *testing.T) {
	ts, _ := newTestServer(t)
	robot := connectRobot(t, ts, "chan-1", wire.ClassRobot)
	browser := pairBrowser(t, robot)
	limited := 0
	commandBurst := int(tierTable[1].CommandBurst) // no sign-in: a home server, tier 1
	for i := 0; i < commandBurst+20; i++ {
		if postCommand(t, browser, ts, "chan-1", "nod") == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited == 0 || limited > 25 {
		t.Fatalf("%d of %d commands limited, want some after a burst of %d", limited, commandBurst+20, commandBurst)
	}
}
