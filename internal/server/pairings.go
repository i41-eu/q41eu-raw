package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/i41-eu/q41eu-raw/e2e"
	"github.com/i41-eu/q41eu-raw/robotauth"
	"github.com/i41-eu/q41eu-raw/wire"
)

// The browsers paired with a robot, as its owner sees them on the manager (robotauth.Seen
// Pairings): which device, since when, last seen, watching now, its end-to-end id. The owner may
// remove them there; the manager's answer then lists them (robotauth.Auth Unpair).

// sessionMeta is what a browser session tells about itself. Kept with the state.
type sessionMeta struct {
	Device string `json:"device,omitempty"` // agentSummary of its User-Agent
	E2E    string `json:"e2e,omitempty"`    // its browser id on the robots (E2EEnroll/E2EHello)
}

// pairLocked pairs a session with a robot (since now). Must hold s.mu.
func (s *Server) pairLocked(session, robotID string) {
	if s.sessions[session] == nil {
		s.sessions[session] = map[string]bool{}
	}
	if s.sessions[session][robotID] {
		return
	}
	s.sessions[session][robotID] = true
	if s.pairedSince[session] == nil {
		s.pairedSince[session] = map[string]time.Time{}
	}
	s.pairedSince[session][robotID] = time.Now().UTC().Truncate(time.Minute)
	s.requestSave()
}

// pairingID is the id the manager gets for a session's pairings: not the session id (a
// credential), but stable for it.
func pairingID(session string) string {
	sum := sha256.Sum256([]byte("i41 pairing|" + session))
	return hex.EncodeToString(sum[:6])
}

// noteE2E records the browser id a session uses with its robots, from an E2EEnroll (its key) or an
// E2EHello (its id).
func (s *Server) noteE2E(session, kind string, body json.RawMessage) {
	var id string
	switch kind {
	case wire.KindE2EEnroll:
		var b wire.E2EEnrollBody
		if json.Unmarshal(body, &b) == nil {
			if pub, err := e2e.ParsePublic(b.B); err == nil {
				id = e2e.BrowserID(pub)
			}
		}
	case wire.KindE2EHello:
		var b wire.E2EHelloBody
		if json.Unmarshal(body, &b) == nil {
			id = b.B
		}
	}
	if len(id) != 2*e2e.BrowserIDBytes {
		return
	}
	if _, err := hex.DecodeString(id); err != nil {
		return
	}
	s.seenMu.Lock()
	m := s.sessionMeta[session]
	changed := m.E2E != id
	m.E2E = id
	s.sessionMeta[session] = m
	s.seenMu.Unlock()
	if changed {
		s.requestSave()
	}
}

// pairings lists the browsers paired with a robot, oldest first.
func (s *Server) pairings(robotID string) []robotauth.Pairing {
	s.seenMu.Lock()
	seen := map[string]time.Time{}
	meta := map[string]sessionMeta{}
	for k, v := range s.sessionSeen {
		seen[k] = v
	}
	for k, v := range s.sessionMeta {
		meta[k] = v
	}
	s.seenMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	watching := map[string]bool{}
	for sub := range s.subs {
		watching[sub.session] = true
	}
	out := []robotauth.Pairing{}
	for session, paired := range s.sessions {
		if !paired[robotID] {
			continue
		}
		out = append(out, robotauth.Pairing{
			ID: pairingID(session), Device: meta[session].Device, Paired: s.pairedSince[session][robotID],
			LastSeen: seen[session], Watching: watching[session], E2E: meta[session].E2E,
		})
	}
	slices.SortFunc(out, func(a, b robotauth.Pairing) int {
		if c := a.Paired.Compare(b.Paired); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

// unpair drops the robot's pairings the owner removed on the manager (by pairing id) and tells the
// robot how many are left.
func (s *Server) unpair(robotID string, ids []string) {
	if len(ids) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dropped := 0
	for session, paired := range s.sessions {
		if paired[robotID] && slices.Contains(ids, pairingID(session)) {
			delete(paired, robotID)
			delete(s.pairedSince[session], robotID)
			dropped++
		}
	}
	if dropped == 0 {
		return
	}
	s.requestSave()
	s.log.Info("pairings removed on the manager", "robot", robotID, "browsers", dropped)
	if rs := s.robots[robotID]; rs != nil && rs.conn != nil {
		if f, err := wire.Marshal(wire.KindPaired, wire.Meta{WorkerID: robotID},
			wire.PairedBody{Viewers: s.viewerCountLocked(robotID), Watching: s.watchingLocked(robotID)}); err == nil {
			rs.conn.enqueue(f)
		}
	}
}
