package server

// Tiers: how much a browser may use a server on the internet (raw.q41.eu). Access to a robot
// does not depend on the tier (browsers pair by the code on the robot's screen, or a robot is
// public); the tier sets the rate limits and the video size.
// When a limit is hit, the answer says how to get more: anonymous people are asked to
// sign in, signed-in people to become sponsors.
//
//	1  the server's admins (-admin-emails) and people listed as tier 1 in the tiers file
//	2  sponsors (listed as tier 2)
//	3  others listed in the file (approved by the owner)
//	4  signed in
//	5  anonymous
//
// A server without sign-in (a home server on the LAN) puts everybody in tier 1.
//
// The tiers file is plain text for a config repository, one rule per line, re-read when it
// changes:
//
//	# tier  who                     (anything after it is a note)
//	1       github:mj41
//	2       github:octocat          sponsor since 2026-10
//	3       email:friend@example.com
//
// "email:" matches an e-mail verified by GitHub or Google (emailTrusted); "github:" a GitHub login and "github-id:" a
// GitHub user id, both only from the GitHub connector of Dex (federated_claims).

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type tierLimits struct {
	CommandsPerSec, CommandBurst float64 // commands, pictures, uploads per browser session
	MediaPerRobot                int     // open media sockets per session and robot
	FullVideo                    bool    // may ask for 640x480 video
}

var tierTable = map[int]tierLimits{
	1: {CommandsPerSec: 20, CommandBurst: 60, MediaPerRobot: 4, FullVideo: true},
	2: {CommandsPerSec: 20, CommandBurst: 60, MediaPerRobot: 4, FullVideo: true},
	3: {CommandsPerSec: 20, CommandBurst: 60, MediaPerRobot: 4, FullVideo: true},
	4: {CommandsPerSec: 10, CommandBurst: 30, MediaPerRobot: 2, FullVideo: false},
	5: {CommandsPerSec: 3, CommandBurst: 15, MediaPerRobot: 1, FullVideo: false},
}

const tiersRecheck = 30 * time.Second

// tierRules is the tiers file, re-read when its modification time changes.
type tierRules struct {
	path string

	mu      sync.Mutex
	checked time.Time
	modTime time.Time
	rules   map[string]int // "email:x@y", "github:login", "github-id:123" -> tier
}

func newTierRules(path string) *tierRules {
	if path == "" {
		return nil
	}
	return &tierRules{path: path, rules: map[string]int{}}
}

// parseTiers reads the rules; a bad line is an error, so a typo does not silently drop someone.
func parseTiers(text string) (map[string]int, error) {
	rules := map[string]int{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for n := 1; sc.Scan(); n++ {
		f := strings.Fields(sc.Text())
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		t, err := strconv.Atoi(f[0])
		if err != nil || t < 1 || t > 3 || len(f) < 2 {
			return nil, fmt.Errorf("line %d: want \"<tier 1-3> <email:|github:|github-id:>who\"", n)
		}
		kind, who, ok := strings.Cut(f[1], ":")
		if !ok || who == "" || (kind != "email" && kind != "github" && kind != "github-id") {
			return nil, fmt.Errorf("line %d: %q is not email:, github: or github-id:", n, f[1])
		}
		key := kind + ":" + strings.ToLower(who)
		if old, dup := rules[key]; !dup || t < old {
			rules[key] = t
		}
	}
	return rules, sc.Err()
}

// current returns the rules, re-reading the file at most every tiersRecheck. A file that
// cannot be read or parsed keeps the rules from before (and is logged).
func (r *tierRules) current(s *Server, now time.Time) map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now.Sub(r.checked) < tiersRecheck {
		return r.rules
	}
	r.checked = now
	fi, err := os.Stat(r.path)
	if err != nil {
		s.log.Warn("tiers file", "path", r.path, "err", err)
		return r.rules
	}
	if fi.ModTime().Equal(r.modTime) {
		return r.rules
	}
	b, err := os.ReadFile(r.path)
	if err == nil {
		var rules map[string]int
		if rules, err = parseTiers(string(b)); err == nil {
			r.rules, r.modTime = rules, fi.ModTime()
			s.log.Info("tiers file loaded", "path", r.path, "rules", len(rules))
			return r.rules
		}
	}
	s.log.Warn("tiers file", "path", r.path, "err", err)
	return r.rules
}

// accountTier is the tier of a signed-in account.
func (s *Server) accountTier(a Account, now time.Time) int {
	if s.isAdmin(a) {
		return 1
	}
	t := 4
	if s.tiers != nil {
		rules := s.tiers.current(s, now)
		keys := []string{}
		if emailTrusted(a) {
			keys = append(keys, "email:"+strings.ToLower(a.Email))
		}
		if a.Provider == "github" {
			if a.Login != "" {
				keys = append(keys, "github:"+strings.ToLower(a.Login))
			}
			if a.ProviderID != "" {
				keys = append(keys, "github-id:"+a.ProviderID)
			}
		}
		for _, k := range keys {
			if rt, ok := rules[k]; ok && rt < t {
				t = rt
			}
		}
	}
	return t
}

// sessionTier is the tier of a browser session.
func (s *Server) sessionTier(session string, now time.Time) int {
	if s.sso == nil {
		return 1 // no sign-in: a home server
	}
	a, ok := s.account(session)
	if !ok {
		return 5
	}
	return s.accountTier(a, now)
}

// tierHint is how to get higher limits, for messages when one is hit.
func (s *Server) tierHint(tier int) string {
	switch tier {
	case 5:
		return "Sign in for higher limits."
	case 4:
		if s.cfg.SponsorURL != "" {
			return "Sponsors get higher limits: " + s.cfg.SponsorURL
		}
		return "Sponsors get higher limits."
	}
	return ""
}

// tooManyText is a refused media socket's close reason (at most 123 bytes): which streams, what to
// do, and for tiers 4 and 5 how to get more.
func (s *Server) tooManyText(session, kind string) string {
	msg := "Too many video and audio streams open for this robot: stop one (or close another tab)."
	if kind == "sensors" {
		msg = "Too many live sensor streams open for this robot: stop one (or close another tab)."
	}
	if hint := s.tierHint(s.sessionTier(session, time.Now())); hint != "" && len(msg)+1+len(hint) <= 123 {
		msg += " " + hint
	}
	return msg
}

// tooMany answers 429 with what was exceeded and, for tiers 4 and 5, how to get more.
func (s *Server) tooMany(w http.ResponseWriter, session, what string) {
	msg := what
	if hint := s.tierHint(s.sessionTier(session, time.Now())); hint != "" {
		msg += ". " + hint
	}
	http.Error(w, msg, http.StatusTooManyRequests)
}
