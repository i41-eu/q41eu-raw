package server

// Sign-in through the Stackchan manager (package sso): one sign-in for every app. The dashboard
// sends a browser without a session to the manager; signed in there, it comes back signed in
// here without a click. A page load tries that silently once after each sign-in at the manager
// (its hint cookie); everyone else just gets the page. Sign-ins are checked with the manager every minute, so
// signing out anywhere signs out here too. Signing in gives no access to any robot by itself:
// browsers pair by the code on the robot's screen, and a robot's owner (the manager says who)
// gets it without a code.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/i41-eu/q41eu-raw/sso"
)

const (
	defaultRobotsPerAccount = 3
	ssoTriedCookie          = "raw_sso_tried" // the manager's hint a silent sign-in was tried with
	signInCheckEvery        = time.Minute
)

// Account is a signed-in person, as the manager knows them (sso.Account): Key (the manager's user
// id) is the same in every app and refreshed at each check (checkSignIns); Email only when the
// provider verified it; Provider, ProviderID and Login are for tiers (tiers.go).
type Account = sso.Account

// ownedInvite is a robot an account added: the SHA-256 of its token, never the token.
type ownedInvite struct {
	RobotID   string    `json:"robot_id"`
	Hash      string    `json:"sha256"`
	Owner     string    `json:"owner"` // Account.Key
	OwnerName string    `json:"owner_name"`
	Created   time.Time `json:"created"`
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// sameOrigin is the CSRF check for requests that change something: browsers send Origin
// on POST and DELETE, and it must be this server.
func sameOrigin(r *http.Request) bool {
	u, err := url.Parse(r.Header.Get("Origin"))
	return err == nil && u.Host != "" && u.Host == r.Host
}

func (s *Server) account(session string) (Account, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.logins[session]
	return a, ok
}

func (s *Server) isAdmin(a Account) bool {
	return emailTrusted(a) && slices.ContainsFunc(s.cfg.AdminEmails, func(e string) bool { return strings.EqualFold(e, a.Email) })
}

// emailTrusted: the e-mail may grant something (admin, a tier). Through Dex only from
// providers that verify it themselves: GitHub and Google. Microsoft's multi-tenant sign-in
// lets any tenant's admin set a user's e-mail ("nOAuth"), so it does not count; neither does
// any connector added later until it is checked and listed here.
func emailTrusted(a Account) bool {
	switch a.Provider {
	case "", "github", "google": // "": not through Dex; email_verified was checked
		return a.Email != ""
	}
	return false
}

// GET /auth/login?next=/path: off to the manager.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.sso == nil {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, s.sso.LoginURL(s.ssoReturn(localPath(r.URL.Query().Get("next"))), false), http.StatusFound)
}

// ssoReturn is this server's address where the manager sends the browser back.
func (s *Server) ssoReturn(next string) string {
	return strings.TrimRight(s.cfg.PublicURL, "/") + "/auth/sso?next=" + url.QueryEscape(next)
}

// localPath is next if it is a path of this site, else "/" (never "//host" or a URL).
func localPath(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\r\n") {
		return "/"
	}
	return next
}

// trySignIn wraps a page: a browser not signed in here, but signed in at the manager lately (its
// hint cookie, sso.HintCookie; or any browser with SignInAlways), goes there silently once and
// comes back signed in.
func (s *Server) trySignIn(page http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.sso == nil || r.URL.Query().Has("signin") {
			page(w, r)
			return
		}
		hint := sso.SilentTry(r, ssoTriedCookie, s.cfg.SignInAlways)
		if hint == "" {
			page(w, r)
			return
		}
		if _, ok := s.account(s.session(w, r)); ok {
			page(w, r)
			return
		}
		sso.MarkTried(w, ssoTriedCookie, hint, strings.HasPrefix(s.cfg.PublicURL, "https://"))
		http.Redirect(w, r, s.sso.LoginURL(s.ssoReturn(r.URL.RequestURI()), true), http.StatusFound)
	}
}

// GET /auth/sso?next=…&code=… (or &error=…): back from the manager.
func (s *Server) handleSSOReturn(w http.ResponseWriter, r *http.Request) {
	if s.sso == nil {
		http.NotFound(w, r)
		return
	}
	session := s.session(w, r)
	q := r.URL.Query()
	next := localPath(q.Get("next"))
	if q.Get("code") == "" { // not signed in at the manager (silent), or cancelled: carry on without
		http.Redirect(w, r, withQuery(next, "signin", "no"), http.StatusSeeOther)
		return
	}
	a, err := s.sso.Exchange(r.Context(), q.Get("code"))
	if err != nil || !a.OK {
		if err != nil {
			s.log.Warn("sign-in failed", "err", err)
		}
		authPage(w, http.StatusBadGateway, "Sign-in failed", "Please try again.")
		return
	}
	s.mu.Lock()
	s.logins[session] = *a.Account
	s.handles[session] = a.Handle
	s.pairOwnedLocked(session, *a.Account)
	s.mu.Unlock()
	s.requestSave()
	s.log.Info("signed in", "account", accountLogID(a.Account.Key))
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// withQuery adds a query parameter to a local path.
func withQuery(path, key, value string) string {
	u, err := url.Parse(path)
	if err != nil {
		return "/"
	}
	q := u.Query()
	q.Set(key, value)
	u.RawQuery = q.Encode()
	return u.String()
}

// RunSignInCheck asks the manager about every sign-in every minute, and ends those it ended
// (signed out there or in another app), until ctx ends.
func (s *Server) RunSignInCheck(ctx context.Context) {
	if s.sso == nil {
		return
	}
	t := time.NewTicker(signInCheckEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.checkSignIns(ctx)
		}
	}
}

func (s *Server) checkSignIns(ctx context.Context) {
	s.mu.Lock()
	handles := make(map[string]string, len(s.handles))
	for session, h := range s.handles {
		handles[session] = h
	}
	s.mu.Unlock()
	for session, h := range handles {
		acct, on, err := s.sso.Check(ctx, h)
		if err != nil {
			s.log.Warn("sign-in check", "err", err)
		}
		if on && acct != nil && acct.Key != "" {
			s.mu.Lock()
			if cur, ok := s.logins[session]; ok && s.handles[session] == h && cur != *acct {
				s.logins[session] = *acct // e.g. the manager joined two sign-ins into one user
				if cur.Key != acct.Key {
					s.pairOwnedLocked(session, *acct)
				}
				s.requestSave()
			}
			s.mu.Unlock()
		}
		if !on {
			s.mu.Lock()
			if s.handles[session] == h {
				delete(s.logins, session)
				delete(s.handles, session)
			}
			s.mu.Unlock()
			s.requestSave()
		}
	}
}

// accountLogID identifies an account in logs without its e-mail or name.
func accountLogID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:6])
}

func authPage(w http.ResponseWriter, status int, title, text string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><meta name="viewport" content="width=device-width,initial-scale=1">`+
		`<title>%s</title><body style="font-family:system-ui;padding:16px"><h1>%s</h1><p>%s</p>`+
		`<p><a href="/?signin=no">Open dashboard</a></p>`, html.EscapeString(title), html.EscapeString(title), html.EscapeString(text))
}

// POST /auth/logout
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	session := s.session(w, r)
	s.mu.Lock()
	h := s.handles[session]
	delete(s.logins, session)
	delete(s.handles, session)
	s.mu.Unlock()
	s.requestSave()
	if s.sso != nil && h != "" { // signing out here signs out of every app
		if err := s.sso.Logout(r.Context(), h); err != nil {
			s.log.Warn("sign-out at the manager", "err", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// robotURL is the address robots connect to: the public URL with ws:// or wss://.
func (s *Server) robotURL() string {
	return strings.Replace(strings.Replace(strings.TrimRight(s.cfg.PublicURL, "/"), "https://", "wss://", 1), "http://", "ws://", 1)
}

// robotURLReachable tells whether a robot can use robotURL: not when the public URL names
// this computer only (localhost, 127.0.0.1, ::1), which on the robot is the robot itself.
func (s *Server) robotURLReachable() bool {
	u, err := url.Parse(s.cfg.PublicURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	if h := u.Hostname(); strings.EqualFold(h, "localhost") || strings.HasSuffix(strings.ToLower(h), ".localhost") {
		return false
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && ip.IsLoopback() {
		return false
	}
	return true
}

// GET /api/me
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	session := s.session(w, r)
	me := map[string]any{"sign_in": s.sso != nil, "manager_url": s.cfg.ManagerURL}
	a, ok := s.account(session)
	if ok {
		s.mu.Lock()
		s.pairOwnedLocked(session, a) // robots that connected since sign-in
		s.mu.Unlock()
		me["name"], me["email"], me["admin"] = a.Name, a.Email, s.isAdmin(a)
	}
	t := s.sessionTier(session, time.Now())
	me["signed_in"], me["tier"], me["tier_hint"], me["full_video"] = ok, t, s.tierHint(t), tierTable[t].FullVideo
	writeJSON(w, http.StatusOK, me)
}
