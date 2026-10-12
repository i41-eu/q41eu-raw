package server

import (
	"context"
	"time"

	"github.com/i41-eu/q41eu-raw/robotauth"
)

// Robots set up by a Stackchan manager carry a token of their own for this app, checked with the
// manager (robotauth); it also says whose robot it is and whether it is public.

// managedRobotLocked records what the manager said about a robot: its owner (whose signed-in
// browsers get it), and whether it is public (a robot that became private loses the pairings of
// everyone else). Must hold s.mu.
func (s *Server) managedRobotLocked(id string, auth robotauth.Auth) {
	s.owned[id] = ownedInvite{RobotID: id, Owner: auth.Owner, OwnerName: auth.OwnerName, Created: time.Now()}
	for session, acct := range s.logins { // the owner's signed-in browsers get it without a code
		if acct.Key == auth.Owner {
			s.pairOwnedLocked(session, acct)
		}
	}
	switch {
	case auth.Public && !s.public[id]:
		s.public[id] = true
	case !auth.Public && s.public[id]:
		delete(s.public, id)
		s.unpairOthersLocked(id)
	}
	s.requestSave()
}

// reportSeen tells the manager which browsers are paired with the robot here and drops the
// pairings the owner removed there. When the robot connects and every managedEvery.
func (s *Server) reportSeen(ctx context.Context, c *robotConn) {
	if s.manager == nil || c.mgrToken == "" {
		return
	}
	auth, err := s.manager.Seen(ctx, c.id, c.mgrToken, robotauth.Seen{Pairings: s.pairings(c.id)})
	if err != nil || !auth.OK {
		return
	}
	s.mu.Lock() // a new owner (two users joined at the manager) or public: now, not at its next connect
	if inv, ok := s.owned[c.id]; auth.Owner != "" && (!ok || inv.Owner != auth.Owner || inv.OwnerName != auth.OwnerName || s.public[c.id] != auth.Public) {
		s.managedRobotLocked(c.id, auth)
	}
	s.mu.Unlock()
	s.unpair(c.id, auth.Unpair)
}

// managedEvery: how often the manager hears about each connected robot's paired browsers (and
// answers with the ones to remove).
const managedEvery = 15 * time.Second

// RunSeenReports reports the connected robots' paired browsers every managedEvery until ctx ends.
func (s *Server) RunSeenReports(ctx context.Context) {
	if s.manager == nil {
		return
	}
	t := time.NewTicker(managedEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.mu.Lock()
			var conns []*robotConn
			for _, rs := range s.robots {
				if rs.conn != nil && rs.conn.mgrToken != "" {
					conns = append(conns, rs.conn)
				}
			}
			s.mu.Unlock()
			for _, c := range conns {
				s.reportSeen(ctx, c)
			}
		}
	}
}
