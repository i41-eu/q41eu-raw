package robotauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// manager answers robot-auth for robot "r1" with token "t1", owned by "o", for the app secret "s".
type manager struct {
	mu    sync.Mutex
	asked int
	down  bool
	ts    *httptest.Server
}

func newManager(t *testing.T) *manager {
	m := &manager{}
	m.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.asked++
		if m.down {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		if r.URL.Path != "/api/robot-auth" || r.Header.Get("Authorization") != "Bearer s" {
			http.Error(w, "unknown app", http.StatusUnauthorized)
			return
		}
		var req struct{ Robot, Token string }
		json.NewDecoder(r.Body).Decode(&req)
		ok := req.Robot == "r1" && req.Token == "t1"
		json.NewEncoder(w).Encode(Auth{OK: ok, Owner: "o", CacheS: 60})
	}))
	t.Cleanup(m.ts.Close)
	return m
}

func (m *manager) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.asked
}

func TestCheck(t *testing.T) {
	if New("", "s") != nil || New("http://x", "") != nil {
		t.Fatal("New without a url or secret: want nil")
	}
	m := newManager(t)
	c := New(m.ts.URL+"/", "s")
	ctx := context.Background()

	if a, err := c.Check(ctx, "r1", "t1"); err != nil || !a.OK || a.Owner != "o" {
		t.Fatalf("right token: %+v %v", a, err)
	}
	c.Check(ctx, "r1", "t1")
	if n := m.count(); n != 1 {
		t.Errorf("a confirmed robot is cached: asked %d times, want 1", n)
	}

	c.Check(ctx, "r1", "wrong")
	c.Check(ctx, "r1", "wrong")
	if n := m.count(); n != 3 {
		t.Errorf("refusals are not cached: asked %d times, want 3", n)
	}

	// The manager goes down: the confirmed robot still connects (with the error), others not.
	m.mu.Lock()
	m.down = true
	m.mu.Unlock()
	c.mu.Lock()
	for k, v := range c.cache {
		v.fresh = time.Now().Add(-time.Second) // stale: ask again
		c.cache[k] = v
	}
	c.mu.Unlock()
	if a, err := c.Check(ctx, "r1", "t1"); err == nil || !a.OK {
		t.Errorf("manager down, robot confirmed recently: %+v %v, want OK with an error", a, err)
	}
	if a, _ := c.Check(ctx, "r2", "t2"); a.OK {
		t.Error("manager down, unknown robot: want not OK")
	}

	if a, err := New(m.ts.URL, "other").Check(ctx, "r1", "t1"); a.OK || err == nil {
		t.Errorf("wrong app secret: %+v %v, want an error", a, err)
	}
}
