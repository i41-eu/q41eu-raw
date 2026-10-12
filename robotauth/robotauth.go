// Package robotauth checks robot tokens with a Stackchan manager (i41eu-manager, e.g.
// m.i41.eu), for the apps it sets robots up for (q41eu-raw, q41eu-pet, ...).
//
// A robot set up by the manager carries a token of its own for each app. When it connects, the
// app asks the manager (POST <manager>/api/robot-auth, authenticated with the app's secret) and
// learns the robot's owner and whether it is public. Answers are cached as long as the manager
// says; refusals are not cached; when the manager cannot be reached, a robot it confirmed within
// the last hour may still connect.
package robotauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	timeout = 10 * time.Second
	grace   = time.Hour // a confirmed robot, while the manager cannot be reached
	maxKeys = 10000
)

// Auth is the manager's answer about a robot and token.
type Auth struct {
	OK        bool   `json:"ok"`
	Owner     string `json:"owner"`
	OwnerName string `json:"owner_name"`
	Public    bool   `json:"public"`
	CacheS    int    `json:"cache_s"`
	// Pairings (Pairing.ID) the owner removed on the manager: the app unpairs those browsers.
	Unpair []string `json:"unpair,omitempty"`
}

type cachedAuth struct {
	auth          Auth
	fresh, usable time.Time // ask again after fresh; use when the manager is down until usable
}

// Client asks one manager. Safe for concurrent use.
type Client struct {
	url, secret string
	http        *http.Client

	mu    sync.Mutex
	cache map[[sha256.Size]byte]cachedAuth
}

// New is a client of the manager at url (e.g. https://m.i41.eu) for the app with this secret;
// nil when either is empty (no manager).
func New(url, secret string) *Client {
	if url == "" || secret == "" {
		return nil
	}
	return &Client{url: strings.TrimRight(url, "/"), secret: secret,
		http: &http.Client{Timeout: timeout}, cache: map[[sha256.Size]byte]cachedAuth{}}
}

// Check asks the manager whether token is robot's token for this app (or answers from the
// cache). With an error, the answer may still be OK: a robot confirmed recently while the
// manager cannot be reached.
func (c *Client) Check(ctx context.Context, robot, token string) (Auth, error) {
	key := sha256.Sum256([]byte(robot + "\x00" + token))
	now := time.Now()
	c.mu.Lock()
	cached, found := c.cache[key]
	c.mu.Unlock()
	if found && now.Before(cached.fresh) {
		return cached.auth, nil
	}
	auth, err := c.ask(ctx, robot, token)
	if err != nil {
		if found && cached.auth.OK && now.Before(cached.usable) {
			return cached.auth, err // the manager is down: a robot it confirmed recently may connect
		}
		return Auth{}, err
	}
	keep := time.Duration(auth.CacheS) * time.Second
	if !auth.OK {
		keep = 0 // a refusal is not cached: a robot set up a moment ago works at once
	}
	c.mu.Lock()
	if len(c.cache) >= maxKeys {
		c.cache = map[[sha256.Size]byte]cachedAuth{}
	}
	c.cache[key] = cachedAuth{auth: auth, fresh: now.Add(keep), usable: now.Add(grace)}
	c.mu.Unlock()
	return auth, nil
}

// Seen tells the manager which browsers are paired with the robot on this app now, and returns
// the manager's answer, fresh (the cache is updated): the pairings its owner removed there (Auth
// Unpair). Apps call it after the robot registers and every 15 s while it stays. (The robot tells
// its manager the rest itself, on its own channel.)
type Seen struct {
	// The browsers paired with the robot on this app (never nil: [] is none), for the owner to
	// see and remove on the manager.
	Pairings []Pairing `json:"pairings"`
}

// Pairing is one browser paired with a robot on an app.
type Pairing struct {
	ID       string    `json:"id"`               // the app's id for it: stable, not a secret
	Device   string    `json:"device,omitempty"` // e.g. "Chrome on Android"
	Paired   time.Time `json:"paired,omitzero"`  // zero: before apps told
	LastSeen time.Time `json:"last_seen,omitzero"`
	Watching bool      `json:"watching,omitempty"` // has the app open now
	// The browser's id on the robot (end-to-end encryption, 16 hex): removing the pairing also
	// makes the robot forget it (a signed Forget from its manager).
	E2E string `json:"e2e,omitempty"`
}

func (c *Client) Seen(ctx context.Context, robot, token string, seen Seen) (Auth, error) {
	if seen.Pairings == nil {
		seen.Pairings = []Pairing{}
	}
	auth, err := c.askWith(ctx, robot, token, &seen)
	if err != nil {
		return c.Check(ctx, robot, token) // the cached answer, or the grace for a confirmed robot
	}
	key := sha256.Sum256([]byte(robot + "\x00" + token))
	if auth.OK {
		c.mu.Lock()
		if len(c.cache) >= maxKeys {
			c.cache = map[[sha256.Size]byte]cachedAuth{}
		}
		now := time.Now()
		c.cache[key] = cachedAuth{auth: auth, fresh: now.Add(time.Duration(auth.CacheS) * time.Second), usable: now.Add(grace)}
		c.mu.Unlock()
	}
	return auth, nil
}

func (c *Client) ask(ctx context.Context, robot, token string) (Auth, error) {
	return c.askWith(ctx, robot, token, nil)
}

func (c *Client) askWith(ctx context.Context, robot, token string, seen *Seen) (Auth, error) {
	body, _ := json.Marshal(struct {
		Robot string `json:"robot"`
		Token string `json:"token"`
		Seen  *Seen  `json:"seen,omitempty"`
	}{robot, token, seen})
	req, err := http.NewRequestWithContext(ctx, "POST", c.url+"/api/robot-auth", bytes.NewReader(body))
	if err != nil {
		return Auth{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.secret)
	resp, err := c.http.Do(req)
	if err != nil {
		return Auth{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Auth{}, fmt.Errorf("manager: %s", resp.Status)
	}
	var auth Auth
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 64<<10)).Decode(&auth); err != nil {
		return Auth{}, fmt.Errorf("manager: %w", err)
	}
	return auth, nil
}
