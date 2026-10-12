package server

// Robot access on a server with sign-in (cfg.ManagerSignIn): every robot is either
// **public** (anyone who sees its screen can pair with the code, as on a server without
// sign-in) or **private**, the default: only its owner, signed in, may pair. The owner of a
// robot an account added is that account; the server's own robots (shared token, or listed
// in -robot-tokens-file) belong to the admins (-admin-emails). Owners' signed-in browsers get
// their robots without a code. Anonymous browsers cannot send join requests, so they cannot
// bother anyone. Without sign-in configured, every robot is public (a LAN server).

import ()

// mayPairLocked reports whether the browser session may pair with robotID. Must hold s.mu.
func (s *Server) mayPairLocked(session, robotID string) bool {
	if s.sso == nil || s.public[robotID] {
		return true
	}
	a, ok := s.logins[session]
	if !ok {
		return false
	}
	return s.ownsLocked(a, robotID)
}

// ownsLocked: a owns robotID (added it, or is an admin and it is not an added robot).
func (s *Server) ownsLocked(a Account, robotID string) bool {
	if inv, ok := s.owned[robotID]; ok {
		return inv.Owner == a.Key
	}
	return s.isAdmin(a)
}

// pairOwnedLocked pairs a signed-in session with the robots its account owns, so owners
// need no code on their own devices. Must hold s.mu.
func (s *Server) pairOwnedLocked(session string, a Account) {
	add := func(id string) { s.pairLocked(session, id) }
	for id, inv := range s.owned {
		if inv.Owner == a.Key {
			add(id)
		}
	}
	if s.isAdmin(a) {
		for id := range s.ownerRobots {
			if _, added := s.owned[id]; !added {
				add(id)
			}
		}
	}
}

// unpairOthersLocked drops the pairings of every session that may no longer pair with
// robotID (after it became private). Must hold s.mu.
func (s *Server) unpairOthersLocked(robotID string) (dropped int) {
	for session, paired := range s.sessions {
		if paired[robotID] && !s.mayPairLocked(session, robotID) {
			delete(paired, robotID)
			delete(s.pairedSince[session], robotID)
			dropped++
		}
	}
	if dropped > 0 {
		s.requestSave()
	}
	return dropped
}
