package server

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

// Join requests: a browser that is not paired yet asks for access, and a
// browser that already is approves it on its own screen. Both show the same
// short code, so the person approving can check it is their request. Approval
// pairs the new browser with the approver's robots. Requests live in memory,
// expire after joinTTL and are rate-limited, so nobody can flood paired
// phones with banners.

const (
	joinTTL        = 3 * time.Minute
	maxJoinPending = 5
	joinPerIPEvery = 10 * time.Second
)

type joinRequest struct {
	ID      string    `json:"id"`
	Code    string    `json:"code"`  // shown on both screens, e.g. "K7M-42P"
	From    string    `json:"from"`  // IP address
	Agent   string    `json:"agent"` // e.g. "Chrome on Linux"
	Expires time.Time `json:"expires"`
	session string
}

// handleJoinRequest (POST /api/join) asks paired browsers to let this one in.
// A browser has at most one pending request; asking again returns it.
func (s *Server) handleJoinRequest(w http.ResponseWriter, r *http.Request) {
	session := s.session(w, r)
	ip := s.clientIP(r) // rate limits are per address
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, signedIn := s.logins[session]; s.sso != nil && !signedIn {
		http.Error(w, "sign in first", http.StatusUnauthorized) // anonymous browsers do not bother anyone
		return
	}
	s.pruneJoins(now)
	for _, j := range s.joins {
		if j.session == session {
			writeJSON(w, http.StatusAccepted, j)
			return
		}
	}
	if !s.hasApprovers(session) {
		http.Error(w, "no browser is paired yet: scan the QR code on the robot first", http.StatusConflict)
		return
	}
	if len(s.joins) >= maxJoinPending || now.Sub(s.joinLastByIP[ip]) < joinPerIPEvery {
		http.Error(w, "too many requests, try again in a minute", http.StatusTooManyRequests)
		return
	}
	b := make([]byte, 8)
	rand.Read(b)
	code := newCode()
	j := &joinRequest{
		ID: hex.EncodeToString(b), Code: code[:3] + "-" + code[3:6], From: ip,
		Agent: agentSummary(r.UserAgent()), Expires: now.Add(joinTTL), session: session,
	}
	s.joins[j.ID] = j
	s.joinLastByIP[ip] = now
	s.publishApprovers(session, sseEvent{name: "join_request", data: mustJSON(j)})
	s.log.Info("join requested", "code", j.Code, "from", ip, "agent", j.Agent)
	writeJSON(w, http.StatusAccepted, j)
}

// handleJoinDecision (POST /api/join/{id}/approve or /deny) is answered by a
// paired browser. Approving pairs the requester with the approver's robots.
func (s *Server) handleJoinDecision(w http.ResponseWriter, r *http.Request) {
	session := s.session(w, r)
	decision := r.PathValue("decision")
	if decision != "approve" && decision != "deny" {
		http.NotFound(w, r)
		return
	}

	s.mu.Lock()
	s.pruneJoins(time.Now())
	j := s.joins[r.PathValue("id")]
	var robots []string
	for id, ok := range s.sessions[session] {
		if ok {
			robots = append(robots, id)
		}
	}
	switch {
	case len(robots) == 0:
		s.mu.Unlock()
		http.Error(w, "only a paired browser can answer", http.StatusForbidden)
		return
	case j == nil:
		s.mu.Unlock()
		http.Error(w, "the request expired or was already answered", http.StatusNotFound)
		return
	case j.session == session:
		s.mu.Unlock()
		http.Error(w, "a browser cannot approve itself", http.StatusForbidden)
		return
	}
	delete(s.joins, j.ID)
	status := "denied"
	if decision == "approve" {
		status = "approved"
		shared := robots[:0:0]
		for _, id := range robots {
			if s.mayPairLocked(j.session, id) { // private robots only to their owner
				s.pairLocked(j.session, id)
				shared = append(shared, id)
			}
		}
		robots = shared
		if len(shared) == 0 {
			status = "denied"
		}
		s.requestSave()
	}
	result := sseEvent{name: "join_result", data: mustJSON(map[string]string{"id": j.ID, "status": status})}
	s.publishSession(j.session, result)
	s.publishApprovers(j.session, sseEvent{name: "join_closed", data: result.data})
	s.mu.Unlock()

	s.log.Info("join "+status, "code", j.Code, "from", j.From, "robots", robots)
	writeJSON(w, http.StatusOK, map[string]string{"status": status})
}

// pruneJoins drops expired requests. Must be called with s.mu held.
func (s *Server) pruneJoins(now time.Time) {
	for id, j := range s.joins {
		if now.After(j.Expires) {
			delete(s.joins, id)
		}
	}
	for ip, t := range s.joinLastByIP {
		if now.Sub(t) > joinPerIPEvery {
			delete(s.joinLastByIP, ip)
		}
	}
}

// pendingJoins returns the open requests a session may answer (none if it has
// no robots). Must be called with s.mu held.
func (s *Server) pendingJoins(session string) []*joinRequest {
	if len(s.sessions[session]) == 0 {
		return nil
	}
	s.pruneJoins(time.Now())
	var out []*joinRequest
	for _, j := range s.joins {
		if j.session != session {
			out = append(out, j)
		}
	}
	return out
}

// hasApprovers reports whether any other paired browser could answer. Must be
// called with s.mu held.
func (s *Server) hasApprovers(except string) bool {
	for session, robots := range s.sessions {
		if session != except && len(robots) > 0 {
			return true
		}
	}
	return false
}

// publishSession sends an event to every open stream of one browser session.
// Must be called with s.mu held.
func (s *Server) publishSession(session string, ev sseEvent) {
	for sub := range s.subs {
		if sub.session == session {
			select {
			case sub.events <- ev:
			default:
			}
		}
	}
}

// publishApprovers sends an event to every paired browser except one. Must be
// called with s.mu held.
func (s *Server) publishApprovers(except string, ev sseEvent) {
	for sub := range s.subs {
		if sub.session != except && len(s.sessions[sub.session]) > 0 {
			select {
			case sub.events <- ev:
			default:
			}
		}
	}
}

// agentSummary turns a User-Agent into "Chrome on Linux" and the like.
func agentSummary(ua string) string {
	browser := "A browser"
	for _, b := range []struct{ token, name string }{
		{"Edg/", "Edge"}, {"OPR/", "Opera"}, {"Firefox/", "Firefox"}, {"Chrome/", "Chrome"}, {"Safari/", "Safari"},
	} {
		if strings.Contains(ua, b.token) {
			browser = b.name
			break
		}
	}
	for _, o := range []struct{ token, name string }{
		{"Android", "Android"}, {"iPhone", "iOS"}, {"iPad", "iOS"}, {"CrOS", "ChromeOS"},
		{"Windows", "Windows"}, {"Mac OS X", "macOS"}, {"Linux", "Linux"},
	} {
		if strings.Contains(ua, o.token) {
			return browser + " on " + o.name
		}
	}
	return browser
}
