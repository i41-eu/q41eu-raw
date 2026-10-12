package server

// End-to-end encrypted traffic (i41eu-arch docs/e2ee.md, package e2e): the server relays
// it between a robot and the browsers paired with it, and cannot read it. What it still
// does is what needs no plaintext: pairing, sessions, who receives what, limits, and the
// camera and microphone switches while someone watches or listens.

import (
	"encoding/json"
	"mime"
	"net/http"
	"time"

	"github.com/i41-eu/q41eu-raw/e2e"
	"github.com/i41-eu/q41eu-raw/wire"
)

// Frame kinds a browser may send to its robot through the relay.
var e2eFromBrowser = map[string]bool{wire.KindE2EEnroll: true, wire.KindE2EHello: true, wire.KindE2ECommand: true}

const maxE2EBody = 16 << 10

// POST /api/robots/{id}/e2e {"kind", "body"}: a browser's sealed frame for its robot.
func (s *Server) handleE2EToRobot(w http.ResponseWriter, r *http.Request) {
	session := s.session(w, r)
	id := r.PathValue("id")
	// Requiring JSON forces a CORS preflight for cross-site requests.
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	if !s.commandAllowed(session, time.Now()) {
		s.tooMany(w, session, "too many requests")
		return
	}
	var req struct {
		Kind string          `json:"kind"`
		Body json.RawMessage `json:"body"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxE2EBody)).Decode(&req); err != nil || !e2eFromBrowser[req.Kind] {
		http.Error(w, `body must be {"kind": "E2EEnroll|E2EHello|E2ECommand", "body": {...}}`, http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	paired := s.sessions[session][id]
	var conn *robotConn
	if st := s.robots[id]; st != nil {
		conn = st.conn
	}
	s.mu.Unlock()
	switch {
	case !paired:
		http.Error(w, "robot not paired with this browser", http.StatusForbidden)
		return
	case conn == nil:
		http.Error(w, "robot is offline", http.StatusConflict)
		return
	}
	s.noteE2E(session, req.Kind, req.Body)
	f, err := wire.Marshal(req.Kind, wire.Meta{WorkerID: id}, req.Body)
	if err != nil || !conn.enqueue(f) {
		http.Error(w, "robot busy, try again", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// relayE2E passes a robot's sealed frame to its paired browsers as the SSE event "e2e".
func (s *Server) relayE2E(robotID string, f wire.Frame) {
	data := mustJSON(map[string]any{"robot": robotID, "kind": f.Kind, "body": f.Body})
	s.mu.Lock()
	s.publish(robotID, "", sseEvent{name: "e2e", data: data})
	s.mu.Unlock()
}

// relayE2EBinary passes a 0x30 message to every media socket open for the robot: the
// inner type (video, audio...) is sealed, so the server cannot pick by it.
func (s *Server) relayE2EBinary(robotID string, msg []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub := range s.media {
		if sub.robot != robotID {
			continue
		}
		select {
		case sub.out <- msg:
		default:
			sub.dropped++
		}
	}
}

// forwardE2EBinary sends a browser's 0x31 message (sealed speaker audio) to the robot.
func (s *Server) forwardE2EBinary(robotID string, msg []byte) bool {
	s.mu.Lock()
	var conn *robotConn
	if st := s.robots[robotID]; st != nil {
		conn = st.conn
	}
	s.mu.Unlock()
	return conn != nil && len(msg) > 1+e2e.BrowserIDBytes+e2e.NonceSize && conn.enqueueBinary(msg)
}
