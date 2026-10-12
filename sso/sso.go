// Package sso signs people in to an app through a Stackchan manager (i41eu-manager, e.g.
// m.i41.eu): one sign-in for every app, and signing out in one signs out of all.
//
// The app sends a browser without a session to LoginURL; the manager sends it back to the
// app's return address with ?code=… (or ?error=login_required, when silent and not signed in
// there). The app trades the code for the account and a handle (Exchange), keeps a session of its
// own, and asks about the handle now and then (Check); its sign-out calls Logout. The manager
// knows the app by the return address (its web address in the manager's catalog) and by the
// app's secret, the same as for robot tokens (package robotauth).
package sso

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HintCookie is set by the manager for its whole domain (e.g. i41.eu, -sign-in-hint-domain) when
// a browser signs in there, and removed when it signs out: a random value, no credential. An app
// on that domain sees it and tries a silent sign-in (SilentHint: once per value, again after RetryAfter), so a person who
// signed in at the manager is signed in at the next page load, and others are never sent there.
const HintCookie = "i41_signed_in"

const (
	timeout = 10 * time.Second
	grace   = time.Hour // a sign-in confirmed within it stays on while the manager cannot be reached
	maxKeys = 10000
)

// Account is the signed-in person, as the manager knows them. Key (the manager's user id, which
// may have several sign-ins) is the same in every app; Email is there only when the provider verified it; Provider is
// the upstream sign-in (Dex's connector id: "github", "google").
type Account struct {
	Key        string `json:"key"`
	Name       string `json:"name"`
	Email      string `json:"email,omitempty"`
	Provider   string `json:"provider,omitempty"`
	ProviderID string `json:"provider_id,omitempty"`
	Login      string `json:"login,omitempty"`
}

// Answer is the manager's answer about a code or a handle.
type Answer struct {
	OK      bool     `json:"ok"`
	Handle  string   `json:"handle,omitempty"`
	Account *Account `json:"account,omitempty"`
	CacheS  int      `json:"cache_s,omitempty"`
}

type cached struct {
	ok            bool
	acct          *Account // as the manager said last
	fresh, usable time.Time
}

// Client talks to one manager for one app. Safe for concurrent use.
type Client struct {
	url, secret string
	http        *http.Client

	mu    sync.Mutex
	cache map[string]cached // handle -> the last answer
}

// New is a client of the manager at managerURL (e.g. https://m.i41.eu) for the app with this
// secret; nil when either is empty.
func New(managerURL, secret string) *Client {
	if managerURL == "" || secret == "" {
		return nil
	}
	return &Client{url: strings.TrimRight(managerURL, "/"), secret: secret,
		http: &http.Client{Timeout: timeout}, cache: map[string]cached{}}
}

// LoginURL is where to send a browser to sign in; it comes back to returnURL (a page of this
// app) with ?code= or ?error=. Silent: never ask the person, only use a sign-in they have.
func (c *Client) LoginURL(returnURL string, silent bool) string {
	q := url.Values{"return": {returnURL}}
	if silent {
		q.Set("silent", "1")
	}
	return c.url + "/sso?" + q.Encode()
}

// Exchange trades a code from the browser's return for the account and its handle.
func (c *Client) Exchange(ctx context.Context, code string) (Answer, error) {
	var a Answer
	if err := c.post(ctx, "token", map[string]string{"code": code}, &a); err != nil {
		return Answer{}, err
	}
	if a.OK && (a.Account == nil || a.Account.Key == "" || a.Handle == "") {
		return Answer{}, fmt.Errorf("manager: incomplete answer")
	}
	if a.OK {
		c.remember(a.Handle, true, a.Account, a.CacheS)
	}
	return a, nil
}

// Check tells whether the sign-in behind handle is still on, with its account as the manager
// knows it now (it changes: a person's sign-ins joined into one user get that user's Key). Answers
// are cached as the manager says. When the manager cannot be reached, a sign-in it confirmed within
// the last hour counts as on, and err says why it was not asked. The account may be nil.
func (c *Client) Check(ctx context.Context, handle string) (*Account, bool, error) {
	now := time.Now()
	c.mu.Lock()
	prev, found := c.cache[handle]
	c.mu.Unlock()
	if found && now.Before(prev.fresh) {
		return prev.acct, prev.ok, nil
	}
	var a Answer
	if err := c.post(ctx, "check", map[string]string{"handle": handle}, &a); err != nil {
		return prev.acct, found && prev.ok && now.Before(prev.usable), err
	}
	c.remember(handle, a.OK, a.Account, a.CacheS)
	return a.Account, a.OK, nil
}

// Logout ends the sign-in at the manager, and so in every app.
func (c *Client) Logout(ctx context.Context, handle string) error {
	c.mu.Lock()
	delete(c.cache, handle)
	c.mu.Unlock()
	return c.post(ctx, "logout", map[string]string{"handle": handle}, nil)
}

func (c *Client) remember(handle string, ok bool, acct *Account, cacheS int) {
	now := time.Now()
	keep := time.Duration(cacheS) * time.Second
	if !ok {
		keep = 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cache) >= maxKeys {
		c.cache = map[string]cached{}
	}
	usable := now.Add(grace)
	if !ok {
		usable = now
	}
	c.cache[handle] = cached{ok: ok, acct: acct, fresh: now.Add(keep), usable: usable}
}

func (c *Client) post(ctx context.Context, what string, body any, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", c.url+"/api/sso/"+what, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.secret)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("manager: %s", resp.Status)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 8<<10)).Decode(out); err != nil {
		return fmt.Errorf("manager: %w", err)
	}
	return nil
}

// SilentHint is the hint to try a silent sign-in with now, or "": a page load of a browser not
// signed in here, with the manager's hint cookie set, and not tried with that hint in the last
// RetryAfter (the app keeps the hint and the time it tried in its cookie triedCookie;
// MarkTried). The hint means the browser is signed in at the manager, so an app that has no
// session for it (lost, or ended here) tries again, but never more often than that: when the
// manager's sign-in is gone too, the try comes back without one and the page just shows.
func SilentHint(r *http.Request, triedCookie string) string {
	return silentHint(r, triedCookie, time.Now(), false)
}

// NoHint is what a silent sign-in is tried with when the manager's hint cookie cannot be seen.
const NoHint = "-"

// SilentTry is SilentHint for an app that cannot see the manager's hint cookie (always: the app
// is on another domain than the manager's -sign-in-hint-domain, e.g. raw.q41.eu and m.i41.eu):
// a browser without the hint is tried too, with NoHint, also at most once per RetryAfter. A
// browser not signed in at the manager comes back without a sign-in and the page shows.
func SilentTry(r *http.Request, triedCookie string, always bool) string {
	return silentHint(r, triedCookie, time.Now(), always)
}

// RetryAfter is how long a silent sign-in tried with one hint is not tried again.
const RetryAfter = 10 * time.Minute

func silentHint(r *http.Request, triedCookie string, now time.Time, always bool) string {
	hint := NoHint
	if c, err := r.Cookie(HintCookie); err == nil && c.Value != "" && len(c.Value) <= 64 && !strings.Contains(c.Value, "|") {
		hint = c.Value
	} else if !always {
		return ""
	}
	if tried, err := r.Cookie(triedCookie); err == nil {
		value, at, _ := strings.Cut(tried.Value, "|")
		sec, _ := strconv.ParseInt(at, 10, 64)
		if value == hint && now.Sub(time.Unix(sec, 0)) < RetryAfter {
			return ""
		}
	}
	return hint
}

// MarkTried remembers in the app's own cookie that the silent sign-in was tried with this hint, now.
func MarkTried(w http.ResponseWriter, triedCookie, hint string, secure bool) {
	http.SetCookie(w, &http.Cookie{Name: triedCookie, Value: hint + "|" + strconv.FormatInt(time.Now().Unix(), 10), Path: "/", MaxAge: 30 * 24 * 3600,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secure})
}
