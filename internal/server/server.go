// Package server relays between Stackchan robots (outbound WebSocket) and
// browsers (HTML + REST + SSE).
//
// Pairing: after a robot registers it gets a short-lived one-time code and
// shows <public-url>/pair?code=... as a QR. The browser that opens that URL
// gets the robot added to its session cookie. Only paired sessions can see
// the robot's telemetry or send it commands.
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/i41-eu/q41eu-raw/robotauth"
	"github.com/i41-eu/q41eu-raw/sso"
	"github.com/i41-eu/q41eu-raw/statestore"
	"github.com/i41-eu/q41eu-raw/wire"
)

//go:embed ui/index.html ui/e2e.js ui/emoji
var uiFS embed.FS

// Config configures a Server.
type Config struct {
	RobotToken      string // shared bearer token robots must present
	RobotTokensFile string // optional per-robot invite tokens (see invites.go); "" disables
	TrustedProxies  int    // reverse proxies in front that append to X-Forwarded-For (see clientIP); 0 ignores it
	NoAddressLimits bool   // the server cannot see client addresses (e.g. behind a TCP load balancer): no per-address limits

	// Robots set up by a Stackchan manager (manager.go): it is asked about their tokens.
	ManagerURL    string // e.g. https://m.i41.eu; "" = no manager
	ManagerSecret string // this app's secret at the manager

	// Sign-in through the manager (accounts.go, package sso), so owners can use their private
	// robots: needs ManagerURL and ManagerSecret. Off: no sign-in, every robot is public.
	ManagerSignIn bool
	// Try a silent sign-in at the manager also without its hint cookie (sso.SilentTry): this app
	// is on another domain than the manager's -sign-in-hint-domain.
	SignInAlways bool
	AdminEmails  []string // verified e-mails of the owners of this server's own robots (shared token)
	TiersFile    string   // tiers 1-3 by e-mail or GitHub login (tiers.go); empty: only admins are tier 1
	SponsorURL   string   // where tier 4 is pointed when a limit is hit

	PublicURL string        // base URL browsers use, e.g. http://192.168.1.10:8765
	PairTTL   time.Duration // lifetime of a pairing code
	UIDir     string        // development: serve index.html from this directory instead of the embedded copy
	StateFile string        // JSON snapshot of pairings and robots, loaded by New (see state.go); "" disables
	// State: where the snapshot goes instead of StateFile (statestore: a file, or a row in
	// Postgres in the cluster).
	State     statestore.Store
	HTTPSPort string // port of the HTTPS listener for browsers, if any; advertised by /api/info
	Log       *slog.Logger
}

// Server holds all state in memory. With Config.StateFile, pairings and known
// robots are also saved to a file and survive a restart (see state.go).
type Server struct {
	cfg Config
	log *slog.Logger

	mu      sync.Mutex
	robots  map[string]*robotState // by robot id; kept after disconnect
	codes   map[string]pairCode    // one-time pairing codes
	invites *robotInvites          // per-robot invite tokens (nil: none)
	manager *robotauth.Client      // nil: no manager

	sso         *sso.Client            // nil: no sign-in (accounts.go)
	logins      map[string]Account     // browser session id -> signed-in account
	handles     map[string]string      // browser session id -> its sign-in's handle at the manager
	owned       map[string]ownedInvite // robot id -> invite added by an account
	ownerRobots map[string]bool        // robot ids seen with the shared token: never claimable
	public      map[string]bool        // robots anyone may pair with by code (access.go); the rest are private

	robotFails  *failLimiter   // failed robot logins per address
	streams     *streamLimiter // open browser streams per session and address
	tiers       *tierRules     // nil without a tiers file
	cmdMu       sync.Mutex
	cmdBuckets  map[string]*bucket         // command budget per browser session
	pairFails   *failLimiter               // wrong pairing codes per address
	sessions    map[string]map[string]bool // browser session id -> paired robot ids
	seenMu      sync.Mutex
	sessionSeen map[string]time.Time            // browser session id -> its last request: pairings unused for pairingTTL go
	sessionMeta map[string]sessionMeta          // browser session id -> its device and e2e id (pairings.go); seenMu
	pairedSince map[string]map[string]time.Time // browser session id -> robot id -> paired at (s.mu)
	subs        map[*subscriber]struct{}        // open SSE streams
	pings       map[string]pendingPing          // "robot/ping id" -> in-flight ping
	media       map[*mediaSub]struct{}          // browser media sockets
	joins       map[string]*joinRequest         // pending join requests by id (join.go)
	// last join request per client IP, for rate limiting
	joinLastByIP map[string]time.Time

	saveNow   chan struct{} // asks RunStateSaver to save soon
	saveMu    sync.Mutex    // serializes SaveState
	lastSaved []byte        // last snapshot written, to skip unchanged saves
}

type pairCode struct {
	robotID string
	expires time.Time
}

type robotState struct {
	id          string
	caps        wire.RobotCapabilities
	labels      map[string]string
	conn        *robotConn // nil while offline
	lastSeen    time.Time
	telemetry   map[string]float64
	telemetryAt time.Time
	cameraOn    bool       // what the server last asked the robot
	cameraFull  bool       // ... at 640x480
	events      []sseEvent // recent robot_event messages, oldest first
	eventSeq    uint64
	// From the robot's "standby" event: when it plans to reconnect.
	standbyUntil time.Time
	micOn        bool
	imuOn        bool   // imu_stream, like cameraOn
	touchOn      bool   // touch_stream
	lightOn      bool   // light_stream
	snapshot     []byte // latest full-resolution still (JPEG), in memory only
	snapshotAt   time.Time
	// Media received from the robot since mediaStatsAt: [0] camera, [1] microphone.
	mediaFrames, mediaBytes [5]int // [0] camera, [1] microphone, [2] IMU, [3] touch, [4] light
	mediaStatsAt            time.Time
}

// robotView is the browser-facing JSON for one robot.
type robotView struct {
	ID          string             `json:"id"`
	Online      bool               `json:"online"`
	Model       string             `json:"model,omitempty"`
	Firmware    string             `json:"firmware,omitempty"`
	Commands    []string           `json:"commands"`
	E2E         bool               `json:"e2e"` // the robot seals its traffic for enrolled browsers (Register label e2e=1)
	Telemetry   map[string]float64 `json:"telemetry"`
	TelemetryAt *time.Time         `json:"telemetry_at,omitempty"`
	// Set while the robot is offline because of the standby command.
	StandbyUntil *time.Time `json:"standby_until,omitempty"`
	LastSeen     time.Time  `json:"last_seen"`
}

type subscriber struct {
	session string
	events  chan sseEvent
}

// sseEvent is one Server-Sent Event: "robot" (full robotView), "pong" or "robot_event".
type sseEvent struct {
	name string
	data []byte
}

func New(cfg Config) *Server {
	if cfg.PairTTL <= 0 {
		cfg.PairTTL = 5 * time.Minute
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	s := &Server{
		cfg:      cfg,
		log:      cfg.Log,
		robots:   map[string]*robotState{},
		codes:    map[string]pairCode{},
		sessions: map[string]map[string]bool{},

		sessionSeen: map[string]time.Time{},
		sessionMeta: map[string]sessionMeta{},
		pairedSince: map[string]map[string]time.Time{},
		subs:        map[*subscriber]struct{}{},
		pings:       map[string]pendingPing{},
		media:       map[*mediaSub]struct{}{},
		joins:       map[string]*joinRequest{},
		saveNow:     make(chan struct{}, 1),

		joinLastByIP: map[string]time.Time{},
		robotFails:   newFailLimiter(maxRobotAuthFails, failWindow, cfg.NoAddressLimits),
		pairFails:    newFailLimiter(maxPairFails, failWindow, cfg.NoAddressLimits),
		streams:      newStreamLimiter(),
		cmdBuckets:   map[string]*bucket{},
		tiers:        newTierRules(cfg.TiersFile),
		logins:       map[string]Account{},
		handles:      map[string]string{},
		owned:        map[string]ownedInvite{},
		ownerRobots:  map[string]bool{},
		public:       map[string]bool{},
	}
	s.manager = robotauth.New(cfg.ManagerURL, cfg.ManagerSecret)
	if cfg.ManagerSignIn {
		s.sso = sso.New(cfg.ManagerURL, cfg.ManagerSecret)
	}
	if cfg.RobotTokensFile != "" {
		s.invites = newRobotInvites(cfg.RobotTokensFile)
		if _, err := s.invites.ok("", "-"); err != nil {
			s.log.Warn("robot tokens file not read", "file", cfg.RobotTokensFile, "err", err)
		}
		s.log.Info("robot invite tokens", "file", cfg.RobotTokensFile, "robots", s.invites.count())
	}
	if s.cfg.State == nil && cfg.StateFile != "" {
		s.cfg.State = &statestore.File{Path: cfg.StateFile}
	}
	if s.cfg.State != nil {
		if err := s.loadState(); err != nil {
			s.log.Warn("state not loaded, starting empty", "state", s.cfg.State.Where(), "err", err)
		}
	}
	return s
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+wire.ConnectPath, s.handleRobotConnect)
	mux.HandleFunc("GET /{$}", s.trySignIn(s.handleIndex))
	mux.HandleFunc("GET /pair", s.trySignIn(s.handlePair))
	mux.HandleFunc("GET /api/robots", s.handleListRobots)
	mux.HandleFunc("GET /api/info", s.handleInfo)
	mux.HandleFunc("GET /e2e.js", s.handleE2EScript)
	mux.HandleFunc("GET /auth/login", s.handleLogin)
	mux.HandleFunc("GET /auth/sso", s.handleSSOReturn)
	mux.HandleFunc("POST /auth/logout", s.handleLogout)
	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("POST /api/join", s.handleJoinRequest)
	mux.HandleFunc("POST /api/join/{id}/{decision}", s.handleJoinDecision)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("POST /api/robots/{id}/command", s.handleCommand)
	mux.HandleFunc("POST /api/robots/{id}/picture", s.handlePicture)
	mux.HandleFunc("POST /api/robots/{id}/e2e", s.handleE2EToRobot)
	mux.HandleFunc("GET /api/robots/{id}/media", s.handleMedia)
	mux.HandleFunc("GET /api/robots/{id}/snapshot", s.handleSnapshot)
	mux.HandleFunc("POST /api/robots/{id}/assets", s.handleAssetUpload)
	mux.Handle("GET /emoji/", http.StripPrefix("/emoji/", s.emojiFiles()))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	return mux
}

// robotAuth checks a connecting robot's token: the shared token (an owner robot), the robot's
// own invite token (-robot-tokens-file), or a token the manager gave it for this app (then the
// manager says whose robot it is). ok is false for none of them; the last two are guests.
func (s *Server) robotAuth(ctx context.Context, id, token string) (ok, guest bool) {
	if s.tokenOK(token) {
		return true, false
	}
	invited, err := s.invites.ok(id, token)
	if err != nil {
		s.log.Warn("robot tokens file", "err", err)
	}
	if invited || s.manager == nil || token == "" {
		return invited, invited
	}
	auth, err := s.manager.Check(ctx, id, token)
	if err != nil {
		s.log.Warn("manager", "robot", id, "err", err)
	}
	if !auth.OK {
		return false, false
	}
	s.mu.Lock()
	s.managedRobotLocked(id, auth)
	s.mu.Unlock()
	return true, true
}

func (s *Server) tokenOK(token string) bool {
	return s.cfg.RobotToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.RobotToken)) == 1
}

/* ------------------------------- robot state ------------------------------ */

// attach marks a robot online with a new connection, replacing any older one.
func (s *Server) attach(c *robotConn, reg wire.RegisterBody) {
	s.mu.Lock()
	st := s.robots[c.id]
	if st == nil {
		st = &robotState{id: c.id, telemetry: map[string]float64{}}
		s.robots[c.id] = st
	}
	old := st.conn
	st.conn = c
	st.caps = reg.Capabilities
	st.labels = reg.Labels
	st.lastSeen = time.Now()
	// A fresh connection starts with camera and mic off (see handleRobotConnect).
	st.cameraOn, st.cameraFull, st.micOn, st.imuOn, st.touchOn, st.lightOn = false, false, false, false, false, false
	st.standbyUntil = time.Time{} // back online
	s.mu.Unlock()

	if old != nil {
		s.log.Info("robot reconnected, closing previous connection", "robot", c.id)
		old.close()
	}
	s.broadcast(c.id)
}

// detach marks a robot offline unless a newer connection already replaced c.
func (s *Server) detach(c *robotConn) {
	s.mu.Lock()
	st := s.robots[c.id]
	if st == nil || st.conn != c {
		s.mu.Unlock()
		return
	}
	st.conn = nil
	for code, pc := range s.codes {
		if pc.robotID == c.id {
			delete(s.codes, code)
		}
	}
	s.mu.Unlock()
	s.broadcast(c.id)
}

func (s *Server) touch(id string) {
	s.mu.Lock()
	if st := s.robots[id]; st != nil {
		st.lastSeen = time.Now()
	}
	s.mu.Unlock()
}

func (s *Server) setTelemetry(id string, m map[string]float64) {
	s.mu.Lock()
	st := s.robots[id]
	if st != nil {
		maps.Copy(st.telemetry, m)
		st.telemetryAt = time.Now()
		st.lastSeen = st.telemetryAt
	}
	s.mu.Unlock()
	if st != nil {
		s.broadcast(id)
	}
}

// view must be called with s.mu held.
func (s *Server) view(st *robotState) robotView {
	v := robotView{
		ID:        st.id,
		Online:    st.conn != nil,
		Model:     st.caps.Model,
		Firmware:  st.caps.Firmware,
		Commands:  slices.Clone(st.caps.Commands),
		E2E:       st.labels["e2e"] == "1",
		Telemetry: make(map[string]float64, len(st.telemetry)),
		LastSeen:  st.lastSeen,
	}
	if v.Commands == nil {
		v.Commands = []string{}
	}
	maps.Copy(v.Telemetry, st.telemetry)
	if !st.telemetryAt.IsZero() {
		t := st.telemetryAt
		v.TelemetryAt = &t
	}
	if st.conn == nil && time.Now().Before(st.standbyUntil) {
		t := st.standbyUntil
		v.StandbyUntil = &t
	}
	return v
}

// broadcast pushes the robot's current state to every SSE stream paired with it.
func (s *Server) broadcast(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.robots[id]
	if st == nil {
		return
	}
	s.publish(id, "", sseEvent{name: "robot", data: mustJSON(s.view(st))})
}

// publish sends ev to SSE streams paired with robotID; with session set, only
// to that browser session. Must be called with s.mu held.
func (s *Server) publish(robotID, session string, ev sseEvent) {
	for sub := range s.subs {
		if !s.sessions[sub.session][robotID] || (session != "" && sub.session != session) {
			continue
		}
		select {
		case sub.events <- ev:
		default: // slow browser; robot state catches up with the next update
		}
	}
}

// robotEvent forwards something that happened on the robot to paired browsers.
// Recent robot events are kept per robot and replayed to browsers that
// connect later (after pairing, after a phone woke up), so an app can still
// see e.g. that the screensaver went on. "seq" lets the browser skip repeats.
// keepRobotEvents is the per-robot history (robot events and sent commands)
// replayed to browsers that connect later.
const keepRobotEvents = 40

func (s *Server) robotEvent(id string, ev wire.RobotEventBody) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.robots[id]
	if st == nil {
		return
	}
	if ev.Name == "standby" {
		if minutes, ok := ev.Data["minutes"].(float64); ok && minutes > 0 {
			st.standbyUntil = time.Now().Add(time.Duration(minutes * float64(time.Minute)))
		}
	}
	st.eventSeq++
	msg := sseEvent{name: "robot_event", data: mustJSON(map[string]any{
		"robot": id, "seq": st.eventSeq, "name": ev.Name, "data": ev.Data, "ts": time.Now(),
	})}
	st.events = append(st.events, msg)
	if len(st.events) > keepRobotEvents {
		st.events = st.events[len(st.events)-keepRobotEvents:]
	}
	s.publish(id, "", msg)
}

// recentEvents returns the kept events of every robot paired with session.
func (s *Server) recentEvents(session string) []sseEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []sseEvent
	for id := range s.sessions[session] {
		if st := s.robots[id]; st != nil {
			out = append(out, st.events...)
		}
	}
	return out
}

/* ---------------------------------- ping ---------------------------------- */

type pendingPing struct {
	session string
	sent    time.Time
}

const pingTimeout = 30 * time.Second

// startPing remembers when a ping left the server and which browser sent it.
func (s *Server) startPing(robotID, pingID, session string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, p := range s.pings {
		if now.Sub(p.sent) > pingTimeout {
			delete(s.pings, k)
		}
	}
	s.pings[robotID+"/"+pingID] = pendingPing{session: session, sent: now}
}

// finishPing answers the browser that sent the ping with the server <-> robot
// round trip; the browser subtracts it from its own total.
func (s *Server) finishPing(robotID string, pong wire.RobotPongBody) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := robotID + "/" + pong.ID
	p, ok := s.pings[key]
	if !ok {
		return
	}
	delete(s.pings, key)
	s.publish(robotID, p.session, sseEvent{name: "pong", data: mustJSON(map[string]any{
		"robot":           robotID,
		"id":              pong.ID,
		"server_robot_ms": float64(time.Since(p.sent).Microseconds()) / 1000,
		"robot_queue_ms":  pong.QueueMs,
	})})
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only called with plain maps and structs
	}
	return b
}

/* --------------------------------- pairing -------------------------------- */

// Unambiguous characters only (no 0/O, 1/I), easy to read off a screen.
const codeAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

func newCode() string {
	b := make([]byte, 8)
	rand.Read(b)
	for i := range b {
		b[i] = codeAlphabet[int(b[i])%len(codeAlphabet)]
	}
	return string(b)
}

// issueCode replaces the robot's pairing code and returns the frame body to send.
func (s *Server) issueCode(robotID string) wire.PairCodeBody {
	code := newCode()
	s.mu.Lock()
	for c, pc := range s.codes {
		if pc.robotID == robotID {
			delete(s.codes, c)
		}
	}
	s.codes[code] = pairCode{robotID: robotID, expires: time.Now().Add(s.cfg.PairTTL)}
	s.mu.Unlock()
	return wire.PairCodeBody{
		Code:       code,
		URL:        s.pairBase() + "/pair?code=" + code,
		ExpiresInS: int(s.cfg.PairTTL / time.Second),
	}
}

// redeem consumes a code and pairs the session with its robot.
func (s *Server) redeem(session, code string) (robotID string, viewers int, conn *robotConn, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pc, found := s.codes[code]
	if !found || time.Now().After(pc.expires) {
		delete(s.codes, code)
		return "", 0, nil, false
	}
	delete(s.codes, code)
	s.pairLocked(session, pc.robotID)
	for _, paired := range s.sessions {
		if paired[pc.robotID] {
			viewers++
		}
	}
	var c *robotConn
	if st := s.robots[pc.robotID]; st != nil {
		c = st.conn
	}
	return pc.robotID, viewers, c, true
}

/* -------------------------------- sessions -------------------------------- */

const sessionCookie = "stackchan_session"

// session returns the browser's session id, setting a new cookie if needed.
// Ids are random 32-byte hex; after a server restart an old cookie stays valid
// but has no paired robots.
func (s *Server) session(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(sessionCookie); err == nil && validSessionID(c.Value) {
		s.seenMu.Lock()
		s.sessionSeen[c.Value] = time.Now().UTC().Truncate(time.Hour) // hours are enough for pairingTTL
		if m := s.sessionMeta[c.Value]; m.Device == "" {
			m.Device = agentSummary(r.UserAgent())
			s.sessionMeta[c.Value] = m
		}
		s.seenMu.Unlock()
		return c.Value
	}
	b := make([]byte, 32)
	rand.Read(b)
	id := hex.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    id,
		Path:     "/",
		MaxAge:   int((365 * 24 * time.Hour) / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// Behind a TLS-terminating gateway r.TLS is nil, so also trust the public URL.
		Secure: r.TLS != nil || strings.HasPrefix(s.cfg.PublicURL, "https://"),
	})
	return id
}

func validSessionID(v string) bool {
	if len(v) != 64 {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil
}

// pairedViews returns the robots paired with a session, sorted by id.
func (s *Server) pairedViews(session string) []robotView {
	s.mu.Lock()
	defer s.mu.Unlock()
	views := []robotView{}
	for id := range s.sessions[session] {
		if st := s.robots[id]; st != nil {
			views = append(views, s.view(st))
		}
	}
	slices.SortFunc(views, func(a, b robotView) int { return strings.Compare(a.ID, b.ID) })
	return views
}

// commandSent records a command a browser sent (or a picture) in the robot's
// history and tells every browser paired with it, so all of them see what was
// sent, not only what the robot reported.
func (s *Server) commandSent(id, command string, args map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.robots[id]
	if st == nil {
		return
	}
	st.eventSeq++
	msg := sseEvent{name: "command_sent", data: mustJSON(map[string]any{
		"robot": id, "seq": st.eventSeq, "command": command, "args": args, "ts": time.Now(),
	})}
	st.events = append(st.events, msg)
	if len(st.events) > keepRobotEvents {
		st.events = st.events[len(st.events)-keepRobotEvents:]
	}
	s.publish(id, "", msg)
}

// viewerCount is the number of browser sessions paired with a robot.
func (s *Server) viewerCount(robotID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.viewerCountLocked(robotID)
}

func (s *Server) viewerCountLocked(robotID string) int {
	n := 0
	for _, paired := range s.sessions {
		if paired[robotID] {
			n++
		}
	}
	return n
}

// watchingLocked: the paired browsers with the app open now (an event stream). s.mu held.
func (s *Server) watchingLocked(robotID string) int {
	seen := map[string]bool{}
	for sub := range s.subs {
		if s.sessions[sub.session][robotID] {
			seen[sub.session] = true
		}
	}
	return len(seen)
}

// tellWatching sends each robot of the session how many browsers are paired and watching now
// (a browser opened or closed the app). s.mu not held.
func (s *Server) tellWatching(session string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.sessions[session] {
		if rs := s.robots[id]; rs != nil && rs.conn != nil {
			if f, err := wire.Marshal(wire.KindPaired, wire.Meta{WorkerID: id},
				wire.PairedBody{Viewers: s.viewerCountLocked(id), Watching: s.watchingLocked(id)}); err == nil {
				rs.conn.enqueue(f)
			}
		}
	}
}

// pairBase is where the robot's QR code sends a browser: the public URL, or, when that is plain
// http and browsers are also served over HTTPS, the HTTPS address on the same host (end-to-end
// encryption works only on a secure page: a phone that pairs over http could not enroll).
func (s *Server) pairBase() string {
	u, err := url.Parse(s.cfg.PublicURL)
	if s.cfg.HTTPSPort == "" || err != nil || u.Scheme != "http" {
		return s.cfg.PublicURL
	}
	return "https://" + net.JoinHostPort(u.Hostname(), s.cfg.HTTPSPort)
}
