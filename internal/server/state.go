package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/i41-eu/q41eu-raw/wire"
)

// State file: pairings and known robots are saved as one JSON snapshot so a
// restart doesn't forget them. A stopgap until there is a real database: the
// whole file is rewritten (at most every stateSaveInterval, right after a
// pairing, and on shutdown) and read once at startup. Robots come back offline
// until they reconnect. Pairing codes and live connections are not saved.

const (
	pairingTTL        = 30 * 24 * time.Hour // a pairing unused this long expires
	stateVersion      = 1
	stateSaveInterval = 5 * time.Second
)

type stateFile struct {
	Version  int                             `json:"version"`
	Sessions map[string][]string             `json:"sessions"`                // browser session id -> paired robot ids
	Seen     map[string]time.Time            `json:"sessions_seen,omitempty"` // their last request (pairingTTL)
	Meta     map[string]sessionMeta          `json:"sessions_meta,omitempty"` // their device and e2e id
	Since    map[string]map[string]time.Time `json:"paired_since,omitempty"`  // session -> robot -> paired at
	Robots   []robotRecord                   `json:"robots"`

	Logins      map[string]Account `json:"logins,omitempty"`        // browser session id -> signed-in account
	Handles     map[string]string  `json:"sso_handles,omitempty"`   // browser session id -> its sign-in's handle at the manager
	Owned       []ownedInvite      `json:"owned_invites,omitempty"` // robots added by accounts (token hashes only)
	OwnerRobots []string           `json:"owner_robots,omitempty"`  // ids seen with the shared token
	Public      []string           `json:"public_robots,omitempty"` // robots anyone may pair with by code
}

type robotRecord struct {
	ID           string                 `json:"id"`
	Capabilities wire.RobotCapabilities `json:"capabilities"`
	Labels       map[string]string      `json:"labels,omitempty"`
	LastSeen     time.Time              `json:"last_seen"`
	Telemetry    map[string]float64     `json:"telemetry,omitempty"`
	TelemetryAt  time.Time              `json:"telemetry_at,omitzero"`
	Events       []json.RawMessage      `json:"events,omitempty"` // robot_event payloads, oldest first
	EventSeq     uint64                 `json:"event_seq"`
	StandbyUntil time.Time              `json:"standby_until,omitzero"`
}

// loadState restores the saved snapshot (cfg.State), if there is one.
func (s *Server) loadState() error {
	b, err := s.cfg.State.Load(context.Background())
	if err != nil {
		return err
	}
	if b == nil {
		return nil
	}
	var st stateFile
	if err := json.Unmarshal(b, &st); err != nil {
		return err
	}
	if st.Version != stateVersion {
		return fmt.Errorf("version %d, want %d", st.Version, stateVersion)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for session, ids := range st.Sessions {
		if !validSessionID(session) {
			continue
		}
		paired := map[string]bool{}
		for _, id := range ids {
			paired[id] = true
		}
		s.sessions[session] = paired
		seen, ok := st.Seen[session]
		if !ok {
			seen = time.Now().UTC() // from before this was kept: a full pairingTTL from now
		}
		s.seenMu.Lock()
		s.sessionSeen[session] = seen
		if m, ok := st.Meta[session]; ok {
			s.sessionMeta[session] = m
		}
		s.seenMu.Unlock()
		if since := st.Since[session]; len(since) > 0 {
			s.pairedSince[session] = since
		}
	}
	for _, r := range st.Robots {
		rs := &robotState{
			id: r.ID, caps: r.Capabilities, labels: r.Labels, lastSeen: r.LastSeen,
			telemetry: r.Telemetry, telemetryAt: r.TelemetryAt, eventSeq: r.EventSeq, standbyUntil: r.StandbyUntil,
		}
		if rs.telemetry == nil {
			rs.telemetry = map[string]float64{}
		}
		for _, ev := range r.Events {
			// The file is indented; an SSE data line must be one line.
			var compact bytes.Buffer
			if json.Compact(&compact, ev) == nil {
				name := "robot_event"
				var probe struct {
					Command string `json:"command"`
				}
				if json.Unmarshal(ev, &probe) == nil && probe.Command != "" {
					name = "command_sent" // see commandSent
				}
				rs.events = append(rs.events, sseEvent{name: name, data: compact.Bytes()})
			}
		}
		s.robots[r.ID] = rs
	}
	for k, v := range st.Logins {
		if h := st.Handles[k]; h != "" && s.sso != nil { // a sign-in without a handle cannot be checked: dropped
			s.logins[k], s.handles[k] = v, h
		}
	}
	for _, inv := range st.Owned {
		s.owned[inv.RobotID] = inv
	}
	for _, id := range st.OwnerRobots {
		s.ownerRobots[id] = true
	}
	for _, id := range st.Public {
		s.public[id] = true
	}
	s.log.Info("state loaded", "state", s.cfg.State.Where(), "sessions", len(st.Sessions), "robots", len(st.Robots))
	return nil
}

func (s *Server) snapshot() ([]byte, error) {
	st := stateFile{Version: stateVersion, Sessions: map[string][]string{}, Seen: map[string]time.Time{},
		Meta: map[string]sessionMeta{}, Since: map[string]map[string]time.Time{}}
	s.seenMu.Lock()
	seen := maps.Clone(s.sessionSeen)
	meta := maps.Clone(s.sessionMeta)
	s.seenMu.Unlock()
	s.mu.Lock()
	for session := range s.sessions { // pairings of browsers not seen for pairingTTL go
		if t, ok := seen[session]; ok && time.Since(t) > pairingTTL {
			delete(s.sessions, session)
			delete(s.pairedSince, session)
		}
	}
	for session, paired := range s.sessions {
		if t, ok := seen[session]; ok {
			st.Seen[session] = t
		}
		if m, ok := meta[session]; ok && len(paired) > 0 {
			st.Meta[session] = m
		}
		for id, t := range s.pairedSince[session] {
			if paired[id] {
				if st.Since[session] == nil {
					st.Since[session] = map[string]time.Time{}
				}
				st.Since[session][id] = t
			}
		}
		for id, ok := range paired {
			if ok {
				st.Sessions[session] = append(st.Sessions[session], id)
			}
		}
		slices.Sort(st.Sessions[session])
	}
	if len(s.logins) > 0 {
		st.Logins = maps.Clone(s.logins)
		st.Handles = maps.Clone(s.handles)
	}
	for _, inv := range s.owned {
		st.Owned = append(st.Owned, inv)
	}
	slices.SortFunc(st.Owned, func(a, b ownedInvite) int { return strings.Compare(a.RobotID, b.RobotID) })
	for id := range s.ownerRobots {
		st.OwnerRobots = append(st.OwnerRobots, id)
	}
	slices.Sort(st.OwnerRobots)
	for id := range s.public {
		st.Public = append(st.Public, id)
	}
	slices.Sort(st.Public)
	for _, r := range s.robots {
		rec := robotRecord{
			ID: r.id, Capabilities: r.caps, Labels: r.labels, LastSeen: r.lastSeen,
			Telemetry: r.telemetry, TelemetryAt: r.telemetryAt, EventSeq: r.eventSeq, StandbyUntil: r.standbyUntil,
		}
		for _, ev := range r.events {
			rec.Events = append(rec.Events, ev.data)
		}
		st.Robots = append(st.Robots, rec)
	}
	slices.SortFunc(st.Robots, func(a, b robotRecord) int { return strings.Compare(a.ID, b.ID) })
	b, err := json.MarshalIndent(st, "", "  ") // under the lock: the maps are shared
	s.mu.Unlock()
	return b, err
}

// SaveState saves the snapshot (cfg.State) if it changed. A file is replaced atomically and
// readable only by this user: session IDs are credentials.
func (s *Server) SaveState() error {
	if s.cfg.State == nil {
		return nil
	}
	b, err := s.snapshot()
	if err != nil {
		return err
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	if bytes.Equal(b, s.lastSaved) {
		return nil
	}
	if err := s.cfg.State.Save(context.Background(), b); err != nil {
		return err
	}
	s.lastSaved = b
	return nil
}

// RunStateSaver saves the state periodically and right after pairings until
// ctx ends. Call SaveState once more after the HTTP server has stopped.
func (s *Server) RunStateSaver(ctx context.Context) {
	if s.cfg.State == nil {
		return
	}
	t := time.NewTicker(stateSaveInterval)
	defer t.Stop()
	lastErr := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.saveNow:
		}
		if err := s.SaveState(); err != nil {
			if err.Error() != lastErr { // e.g. a read-only file system: say it once
				s.log.Warn("state not saved", "state", s.cfg.State.Where(), "err", err)
			}
			lastErr = err.Error()
		} else {
			lastErr = ""
		}
	}
}

// requestSave asks RunStateSaver to save soon. Safe with s.mu held.
func (s *Server) requestSave() {
	select {
	case s.saveNow <- struct{}{}:
	default:
	}
}
