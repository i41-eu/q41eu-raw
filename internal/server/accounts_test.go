package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/i41-eu/q41eu-raw/robotauth"
	"github.com/i41-eu/q41eu-raw/sso"
	"github.com/i41-eu/q41eu-raw/wire"
)

// user is one browser with its own cookies.
type user struct {
	t      *testing.T
	c      *http.Client
	server string
}

func newUser(t *testing.T, server string) *user {
	jar, _ := cookiejar.New(nil)
	return &user{t: t, server: server, c: &http.Client{Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// signIn signs in at the (fake) manager as sub, then here: /auth/login, the manager's /sso,
// back to /auth/sso with a code.
func (u *user) signIn(m *fakeManager, sub, email, name string) {
	u.t.Helper()
	m.mu.Lock()
	m.next = &sso.Account{Key: m.ts.URL + "|" + sub, Name: name, Email: email, Provider: "github"}
	m.mu.Unlock()
	resp, err := u.c.Get(u.server + "/auth/login")
	if err != nil || resp.StatusCode != http.StatusFound || !strings.HasPrefix(resp.Header.Get("Location"), m.ts.URL+"/sso?") {
		u.t.Fatalf("login: %v %v", err, resp.Header.Get("Location"))
	}
	resp, err = u.c.Get(resp.Header.Get("Location"))
	if err != nil || resp.StatusCode != http.StatusFound {
		u.t.Fatalf("manager: %v %v", err, resp)
	}
	back := u.local(resp.Header.Get("Location"))
	resp, err = u.c.Get(back)
	if err != nil || resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		u.t.Fatalf("back from the manager: %v %d %s", err, resp.StatusCode, body)
	}
}

// local is a URL of this server's public address (https://chan.example) on the test server.
func (u *user) local(public string) string {
	return strings.Replace(public, "https://chan.example", u.server, 1)
}

func (u *user) do(method, path, body string, sameOrigin bool) (int, string) {
	u.t.Helper()
	req, _ := http.NewRequest(method, u.server+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if sameOrigin {
		req.Header.Set("Origin", u.server)
	}
	resp, err := u.c.Do(req)
	if err != nil {
		u.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// fakeManager answers like a Stackchan manager: robot-auth from a table the test controls, and
// the sign-on (/sso, /api/sso/*) for the account the test puts in next.
type fakeManager struct {
	ts     *httptest.Server
	mu     sync.Mutex
	robots map[string]fakeManaged // robot id -> its token for this app and owner
	asked  int

	pairings map[string][]robotauth.Pairing // robot id -> what the app last reported
	unpair   []string                       // the answer's Unpair

	next    *sso.Account           // the next browser to come signs in as (nil: nobody)
	codes   map[string]sso.Account // one-time codes
	handles map[string]sso.Account // handles of sign-ins that are on
	logouts int
	noCache bool // answers say "do not keep" (cache_s 0): every check asks
}

type fakeManaged struct {
	token, owner string
	public       bool
}

const managerSecret = "app-secret"

func newFakeManager(t *testing.T) *fakeManager {
	m := &fakeManager{robots: map[string]fakeManaged{}, codes: map[string]sso.Account{}, handles: map[string]sso.Account{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sso", func(w http.ResponseWriter, r *http.Request) {
		ret := r.URL.Query().Get("return")
		m.mu.Lock()
		defer m.mu.Unlock()
		switch {
		case m.next != nil:
			code := fmt.Sprintf("code-%d", len(m.codes)+1)
			m.codes[code] = *m.next
			m.next = nil // one browser: the next one is not signed in there
			http.Redirect(w, r, ret+"&code="+code, http.StatusFound)
		case r.URL.Query().Get("silent") == "1":
			http.Redirect(w, r, ret+"&error=login_required", http.StatusFound)
		default:
			http.Error(w, "the manager's sign-in page", http.StatusOK)
		}
	})
	mux.HandleFunc("POST /api/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+managerSecret {
			http.Error(w, "unknown app", http.StatusUnauthorized)
			return
		}
		var req struct {
			Robot, Token, Code, Handle string
			Seen                       *robotauth.Seen
		}
		json.NewDecoder(r.Body).Decode(&req)
		m.mu.Lock()
		defer m.mu.Unlock()
		switch r.URL.Path {
		case "/api/robot-auth":
			m.asked++
			rb, ok := m.robots[req.Robot]
			if !ok || rb.token != req.Token {
				json.NewEncoder(w).Encode(map[string]any{"ok": false})
				return
			}
			if req.Seen != nil {
				if m.pairings == nil {
					m.pairings = map[string][]robotauth.Pairing{}
				}
				m.pairings[req.Robot] = req.Seen.Pairings
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "owner": rb.owner, "owner_name": "x", "public": rb.public,
				"unpair": m.unpair})
		case "/api/sso/token":
			a, ok := m.codes[req.Code]
			delete(m.codes, req.Code)
			if !ok {
				json.NewEncoder(w).Encode(sso.Answer{})
				return
			}
			h := "handle-" + req.Code
			m.handles[h] = a
			cacheS := 60
			if m.noCache {
				cacheS = 0
			}
			json.NewEncoder(w).Encode(sso.Answer{OK: true, Handle: h, Account: &a, CacheS: cacheS})
		case "/api/sso/check":
			a, ok := m.handles[req.Handle]
			json.NewEncoder(w).Encode(sso.Answer{OK: ok, Handle: req.Handle, Account: &a})
		case "/api/sso/logout":
			delete(m.handles, req.Handle)
			m.logouts++
			w.WriteHeader(http.StatusNoContent)
		}
	})
	m.ts = httptest.NewServer(mux)
	t.Cleanup(m.ts.Close)
	return m
}

// signOutEverywhere ends every sign-in at the manager (as if signed out on the manager's page).
func (m *fakeManager) signOutEverywhere() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next = nil
	m.handles = map[string]sso.Account{}
}

// set gives a robot a token for this app, owned by owner (an account key: the manager's user id).
func (m *fakeManager) set(id, token, owner string, public bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.robots[id] = fakeManaged{token: token, owner: owner, public: public}
}

func newSignInServer(t *testing.T, m *fakeManager, stateFile string) (*httptest.Server, *Server) {
	ts, s, _ := newManagedServer(t, m, stateFile)
	return ts, s
}

// newManagedServer is a server with a (fake) manager for robot tokens and sign-in.
func newManagedServer(t *testing.T, m *fakeManager, stateFile string) (*httptest.Server, *Server, *fakeManager) {
	t.Helper()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(Config{RobotToken: testToken, PairTTL: time.Minute, Log: quiet, StateFile: stateFile,
		ManagerURL: m.ts.URL, ManagerSecret: managerSecret, ManagerSignIn: true,
		AdminEmails: []string{"boss@example.com"}})
	ts := httptest.NewServer(s.Handler())
	s.cfg.PublicURL = "https://chan.example"
	t.Cleanup(ts.Close)
	return ts, s, m
}

// A robot set up by the manager connects with its token for this app; the manager says whose
// it is. Wrong tokens and unknown robots are refused; a confirmed robot keeps working for a
// while when the manager is down; a refusal is never cached.
func TestManagedRobot(t *testing.T) {
	f := newFakeManager(t)
	ts, _, m := newManagedServer(t, f, "")
	const id = "stackchan-0a1b2c3d4e50"
	if got := dialStatus(ts, "tok-1", id); got != http.StatusUnauthorized {
		t.Fatalf("unknown to the manager: %d", got)
	}
	m.set(id, "tok-1", f.ts.URL+"|ema", false)
	registerGuest(t, ts, "tok-1", id) // refused a moment ago, works now: refusals are not cached
	if got := dialStatus(ts, "tok-2", id); got != http.StatusUnauthorized {
		t.Fatalf("a wrong token: %d", got)
	}
	ema := newUser(t, ts.URL)
	ema.signIn(f, "ema", "ema@example.com", "Ema")
	if got := ema.pairedIDs(); got != id {
		t.Fatalf("the owner (from the manager) has the robot: %q", got)
	}
	// The manager goes away: the robot it confirmed still connects (cached).
	m.ts.Close()
	registerGuest(t, ts, "tok-1", id)
}

// Back from the manager without a code (not signed in there, silent) or with a bad one: not
// signed in; never sent anywhere but a path of this site.
func TestSignInReturnChecks(t *testing.T) {
	m := newFakeManager(t)
	ts, _ := newSignInServer(t, m, "")
	u := newUser(t, ts.URL)
	resp, _ := u.c.Get(ts.URL + "/auth/sso?next=%2F%2Fevil.example&error=login_required")
	if loc := resp.Header.Get("Location"); loc != "/?signin=no" {
		t.Errorf("error, next //evil.example: to %q", loc)
	}
	if code, _ := u.do("GET", "/auth/sso?next=%2F&code=unknown", "", false); code != http.StatusBadGateway {
		t.Errorf("an unknown code: %d", code)
	}
	if _, me := u.do("GET", "/api/me", "", false); !strings.Contains(me, `"signed_in":false`) {
		t.Errorf("signed in after all: %s", me)
	}
}

// A page load goes to the manager silently only with the manager's hint cookie (signed in there
// lately), once per hint: no hint, no trip; signed in there, back signed in without a click.
func TestSilentSignIn(t *testing.T) {
	m := newFakeManager(t)
	ts, _ := newSignInServer(t, m, "")
	u := newUser(t, ts.URL)
	follow := func(path string) (int, string, int) {
		resp, err := u.c.Get(u.local(path))
		if err != nil {
			t.Fatal(err)
		}
		hops := 0
		for ; hops < 5 && (resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusSeeOther); hops++ {
			loc := resp.Header.Get("Location")
			if strings.HasPrefix(loc, "/") {
				loc = ts.URL + loc
			}
			if resp, err = u.c.Get(u.local(loc)); err != nil {
				t.Fatal(err)
			}
		}
		return resp.StatusCode, resp.Request.URL.RequestURI(), hops
	}
	hint := func(v string) {
		su, _ := url.Parse(ts.URL)
		u.c.Jar.SetCookies(su, []*http.Cookie{{Name: sso.HintCookie, Value: v, Path: "/"}})
	}
	if _, _, hops := follow(ts.URL + "/pair?code=X"); hops != 0 { // (X is no valid code: 400)
		t.Fatalf("no hint: %d hops", hops)
	}
	hint("g1") // signed in at the manager once, signed out since
	if _, at, hops := follow(ts.URL + "/pair?code=X"); hops == 0 || !strings.HasPrefix(at, "/pair?") || !strings.Contains(at, "signin=no") {
		t.Fatalf("hint, not signed in there: at %s after %d hops", at, hops)
	}
	if _, _, hops := follow(ts.URL + "/"); hops != 0 {
		t.Fatalf("the same hint again: %d hops", hops)
	}
	// Signed in at the manager again (a new hint): signed in here at the next page load.
	m.mu.Lock()
	m.next = &sso.Account{Key: m.ts.URL + "|ema", Name: "Ema"}
	m.mu.Unlock()
	hint("g2")
	follow(ts.URL + "/")
	if _, me := u.do("GET", "/api/me", "", false); !strings.Contains(me, `"signed_in":true`) {
		t.Fatalf("a new hint, signed in at the manager: %s", me)
	}
}

// With SignInAlways (the app on another domain than the manager's hint cookie) a page load goes
// to the manager silently without a hint too, once per RetryAfter: signed in there, back signed
// in; not, back without a sign-in and not sent again at the next page load.
func TestSilentSignInAlways(t *testing.T) {
	m := newFakeManager(t)
	ts, s := newSignInServer(t, m, "")
	s.cfg.SignInAlways = true
	follow := func(u *user, path string) (string, int) {
		resp, err := u.c.Get(u.local(path))
		if err != nil {
			t.Fatal(err)
		}
		hops := 0
		for ; hops < 5 && (resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusSeeOther); hops++ {
			loc := resp.Header.Get("Location")
			if strings.HasPrefix(loc, "/") {
				loc = ts.URL + loc
			}
			if resp, err = u.c.Get(u.local(loc)); err != nil {
				t.Fatal(err)
			}
		}
		return resp.Request.URL.RequestURI(), hops
	}
	u := newUser(t, ts.URL)
	if at, hops := follow(u, ts.URL+"/"); hops == 0 || !strings.Contains(at, "signin=no") {
		t.Fatalf("no hint, not signed in there: at %s after %d hops", at, hops)
	}
	if _, hops := follow(u, ts.URL+"/"); hops != 0 {
		t.Fatalf("tried a moment ago: %d hops", hops)
	}
	// Another browser, signed in at the manager: signed in here at its first page load.
	m.mu.Lock()
	m.next = &sso.Account{Key: m.ts.URL + "|ema", Name: "Ema"}
	m.mu.Unlock()
	v := newUser(t, ts.URL)
	follow(v, ts.URL+"/")
	if _, me := v.do("GET", "/api/me", "", false); !strings.Contains(me, `"signed_in":true`) {
		t.Fatalf("signed in at the manager, no hint: %s", me)
	}
}

// Signing out at the manager (or in another app) signs out here within a check; signing out here
// signs out at the manager.
func TestSignOutEverywhere(t *testing.T) {
	m := newFakeManager(t)
	ts, s := newSignInServer(t, m, "")
	u := newUser(t, ts.URL)
	u.signIn(m, "ema", "ema@example.com", "Ema")
	m.signOutEverywhere()
	s.sso = sso.New(m.ts.URL, managerSecret) // no cached answer
	s.checkSignIns(context.Background())
	if _, me := u.do("GET", "/api/me", "", false); !strings.Contains(me, `"signed_in":false`) {
		t.Fatalf("signed out at the manager, still here: %s", me)
	}
	u.signIn(m, "ema", "ema@example.com", "Ema")
	if code, _ := u.do("POST", "/auth/logout", "", true); code != http.StatusNoContent {
		t.Fatalf("logout: %d", code)
	}
	m.mu.Lock()
	n, left := m.logouts, len(m.handles)
	m.mu.Unlock()
	if n != 1 || left != 0 {
		t.Errorf("signing out here: %d logouts at the manager, %d sign-ins left there", n, left)
	}
}

func TestRobotURL(t *testing.T) {
	for _, c := range []struct {
		public, robot string
		reachable     bool
	}{
		{"https://chan.example/", "wss://chan.example", true},
		{"http://192.168.1.10:8765", "ws://192.168.1.10:8765", true},
		{"http://localhost:8765", "ws://localhost:8765", false},
		{"http://127.0.0.1:8799", "ws://127.0.0.1:8799", false},
		{"http://[::1]:8765", "ws://[::1]:8765", false},
		{"", "", false},
	} {
		s := &Server{cfg: Config{PublicURL: c.public}}
		if got := s.robotURL(); got != c.robot {
			t.Errorf("robotURL(%q) = %q, want %q", c.public, got, c.robot)
		}
		if got := s.robotURLReachable(); got != c.reachable {
			t.Errorf("robotURLReachable(%q) = %v, want %v", c.public, got, c.reachable)
		}
	}
}

// The app tells the manager who is paired with a robot (device, since, end-to-end id) and drops
// the pairings the owner removed there.
func TestPairingsReportedAndRemoved(t *testing.T) {
	_, s, m := newManagedServer(t, newFakeManager(t), "")
	const robot = "stackchan-0a1b2c3d4e50"
	m.set(robot, "tok-0", "o", false)
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	s.seenMu.Lock()
	s.sessionMeta[a] = sessionMeta{Device: "Chrome on Android"}
	s.seenMu.Unlock()
	s.mu.Lock()
	s.pairLocked(a, robot)
	s.pairLocked(b, robot)
	s.mu.Unlock()
	s.noteE2E(b, wire.KindE2EHello, json.RawMessage(`{"b":"0011223344556677"}`))
	c := &robotConn{id: robot, mgrToken: "tok-0"}
	s.reportSeen(context.Background(), c)
	m.mu.Lock()
	got := m.pairings[robot]
	m.mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("reported %+v", got)
	}
	byID := map[string]robotauth.Pairing{}
	for _, p := range got {
		byID[p.ID] = p
	}
	pa, pb := byID[pairingID(a)], byID[pairingID(b)]
	if pa.Device != "Chrome on Android" || pa.Paired.IsZero() || pb.E2E != "0011223344556677" || pa.E2E != "" {
		t.Errorf("a %+v, b %+v", pa, pb)
	}
	if strings.Contains(fmt.Sprint(got), a) {
		t.Error("a session id reported")
	}
	m.mu.Lock()
	m.unpair = []string{pairingID(b)}
	m.mu.Unlock()
	s.reportSeen(context.Background(), c)
	s.mu.Lock()
	left := s.sessions[a][robot] && !s.sessions[b][robot]
	s.mu.Unlock()
	if !left {
		t.Error("b not unpaired, or a too")
	}
	if p := s.pairings(robot); len(p) != 1 || p[0].ID != pairingID(a) {
		t.Errorf("after: %+v", p)
	}
}
