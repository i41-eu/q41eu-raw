package server

import (
	"encoding/json"
	"fmt"
	"html"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/i41-eu/q41eu-raw/wire"
)

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	s.servePage(w, r, "index.html")
}

// handleE2EScript (GET /e2e.js): the browser side of end-to-end encryption.
func (s *Server) handleE2EScript(w http.ResponseWriter, r *http.Request) {
	js, err := uiFS.ReadFile("ui/e2e.js")
	if s.cfg.UIDir != "" {
		js, err = os.ReadFile(filepath.Join(s.cfg.UIDir, "e2e.js"))
	}
	if err != nil {
		http.Error(w, "ui missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(js)
}

var fileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

func (s *Server) servePage(w http.ResponseWriter, r *http.Request, name string) {
	s.session(w, r)
	page, err := uiFS.ReadFile("ui/" + name)
	if s.cfg.UIDir != "" {
		page, err = os.ReadFile(filepath.Join(s.cfg.UIDir, name))
	}
	if err != nil {
		http.Error(w, "ui missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(page)
}

// handleInfo tells the dashboard about this server: the HTTPS address of the
// same host when there is an HTTPS listener (the page links to it for the microphone).
func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	info := map[string]string{}
	if s.cfg.HTTPSPort != "" {
		host := r.Host
		if h, _, err := net.SplitHostPort(r.Host); err == nil {
			host = h
		}
		info["https_url"] = "https://" + net.JoinHostPort(host, s.cfg.HTTPSPort)
	}
	writeJSON(w, http.StatusOK, info)
}

// emojiFiles serves the dashboard's emoji SVGs (Fluent Emoji Flat, MIT; see
// ui/emoji/LICENSE). They need no session: they are public artwork.
func (s *Server) emojiFiles() http.Handler {
	if s.cfg.UIDir != "" {
		return http.FileServer(http.Dir(filepath.Join(s.cfg.UIDir, "emoji")))
	}
	sub, _ := fs.Sub(uiFS, "ui/emoji")
	return http.FileServerFS(sub)
}

// handlePair is the QR target. GET because a phone camera opens it directly;
// the one-time code itself is the proof that the user can see the robot.
func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	session := s.session(w, r)
	code := strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(r.URL.Query().Get("code")))
	ip, now := s.clientIP(r), time.Now()
	if s.pairFails.blocked(ip, now) {
		http.Error(w, "too many wrong codes from this address, try again later", http.StatusTooManyRequests)
		return
	}
	// A private robot pairs only with its owner, signed in; the code stays valid for them.
	s.mu.Lock()
	pc, known := s.codes[code]
	private := known && !s.mayPairLocked(session, pc.robotID)
	s.mu.Unlock()
	if private {
		next := "/auth/login?next=" + url.QueryEscape("/pair?code="+code)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, `<!doctype html><meta name="viewport" content="width=device-width,initial-scale=1">`+
			`<title>This robot is private</title><body style="font-family:system-ui;padding:16px">`+
			`<h1>This robot is private</h1><p>Only its owner can pair with it: <a href="%s">sign in</a> `+
			`with the account that added it.</p><p><a href="/?signin=no">Open dashboard</a></p>`+
			// An encrypted robot's QR code carries its key in the fragment (#e2e=…), which a
			// sign-in's redirects lose: the dashboard takes it from this tab after pairing.
			`<script>try{if(location.hash.startsWith("#e2e="))sessionStorage.setItem("e2e-pair",JSON.stringify({hash:location.hash,at:Date.now()}))}catch(e){}</script>`,
			html.EscapeString(next))
		return
	}

	robotID, viewers, conn, ok := s.redeem(session, code)
	if !ok {
		s.pairFails.fail(ip, now)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `<!doctype html><meta name="viewport" content="width=device-width,initial-scale=1">`+
			`<title>Pairing failed</title><body style="font-family:system-ui;padding:16px">`+
			`<h1>Pairing failed</h1><p>This code is invalid, expired, or already used. `+
			`Scan the QR code on the robot again.</p><p><a href="/">Open dashboard</a></p>`)
		return
	}
	s.log.Info("browser paired", "robot", robotID, "viewers", viewers)

	// Tell the robot, and replace the used code so the QR on screen stays valid.
	if conn != nil {
		s.mu.Lock()
		watching := s.watchingLocked(robotID)
		s.mu.Unlock()
		if f, err := wire.Marshal(wire.KindPaired, wire.Meta{WorkerID: robotID}, wire.PairedBody{Viewers: viewers, Watching: watching}); err == nil {
			conn.enqueue(f)
		}
		s.sendPairCode(conn)
	}
	s.broadcast(robotID)
	http.Redirect(w, r, "/?paired="+url.QueryEscape(robotID), http.StatusSeeOther)
}

func (s *Server) handleListRobots(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.pairedViews(s.session(w, r)))
}

// handleEvents streams SSE: "robot" (full robotView), "pong" (only to the
// browser that sent the ping) and "robot_event".
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	session := s.session(w, r)
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")

	keys := s.streamKeys("sse", session, s.clientIP(r), maxSSEPerSession, maxSSEPerAddr)
	if !s.streams.acquire(keys) {
		http.Error(w, "too many open event streams", http.StatusTooManyRequests)
		return
	}
	defer s.streams.release(keys)

	sub := &subscriber{session: session, events: make(chan sseEvent, 32)}
	s.mu.Lock()
	s.subs[sub] = struct{}{}
	s.mu.Unlock()
	s.tellWatching(session) // the robots show how many watch now
	defer func() {
		s.mu.Lock()
		delete(s.subs, sub)
		s.mu.Unlock()
		s.tellWatching(session)
	}()

	send := func(ev sseEvent) bool {
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.name, ev.data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	for _, v := range s.pairedViews(session) {
		if !send(sseEvent{name: "robot", data: mustJSON(v)}) {
			return
		}
	}
	for _, ev := range s.recentEvents(session) {
		if !send(ev) {
			return
		}
	}
	s.mu.Lock()
	joins := s.pendingJoins(session)
	s.mu.Unlock()
	for _, j := range joins { // requests that arrived while this browser was away
		if !send(sseEvent{name: "join_request", data: mustJSON(j)}) {
			return
		}
	}
	fmt.Fprint(w, ": ready\n\n")
	flusher.Flush()

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-sub.events:
			if !send(ev) {
				return
			}
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Server) handleCommand(w http.ResponseWriter, r *http.Request) {
	session := s.session(w, r)
	id := r.PathValue("id")
	if !s.commandAllowed(session, time.Now()) {
		s.tooMany(w, session, "too many commands")
		return
	}

	// Requiring JSON forces a CORS preflight for cross-site requests.
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	var cmd wire.RobotCommandBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&cmd); err != nil || cmd.Command == "" { // long raw IR codes
		http.Error(w, "body must be {\"command\": \"...\"}", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	paired := s.sessions[session][id]
	st := s.robots[id]
	var conn *robotConn
	var commands []string
	if st != nil {
		conn = st.conn
		commands = st.caps.Commands
	}
	s.mu.Unlock()

	switch {
	case !paired || st == nil:
		http.Error(w, "robot not paired with this browser", http.StatusForbidden)
		return
	case !slices.Contains(commands, cmd.Command):
		http.Error(w, "robot does not support command "+cmd.Command, http.StatusBadRequest)
		return
	case conn == nil:
		http.Error(w, "robot is offline", http.StatusConflict)
		return
	}

	if cmd.Command == "ping" {
		pingID, _ := cmd.Args["id"].(string)
		if !pingIDPattern.MatchString(pingID) {
			http.Error(w, "ping needs args.id: 1-32 letters or digits", http.StatusBadRequest)
			return
		}
		s.startPing(id, pingID, session)
	}

	f, err := wire.Marshal(wire.KindRobotCommand, wire.Meta{WorkerID: id}, cmd)
	if err != nil || !conn.enqueue(f) {
		http.Error(w, "robot is not accepting commands", http.StatusServiceUnavailable)
		return
	}
	if cmd.Command == "ping" {
		s.log.Debug("command sent", "robot", id, "command", cmd.Command)
	} else {
		s.log.Info("command sent", "robot", id, "command", cmd.Command)
		s.commandSent(id, cmd.Command, cmd.Args)
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "sent"})
}

var pingIDPattern = regexp.MustCompile(`^[A-Za-z0-9]{1,32}$`)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
