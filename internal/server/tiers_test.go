package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseTiers(t *testing.T) {
	rules, err := parseTiers(`
# tier who
1 github:MJ41
2 github:octocat   sponsor since 2026-10
3 email:Friend@Example.com
3 github:octocat
1 github-id:583231
`)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"github:mj41": 1, "github:octocat": 2, "email:friend@example.com": 3, "github-id:583231": 1}
	if len(rules) != len(want) {
		t.Fatalf("rules %v, want %v", rules, want)
	}
	for k, v := range want {
		if rules[k] != v {
			t.Fatalf("rules %v, want %v", rules, want)
		}
	}
	for _, bad := range []string{"4 github:x", "0 email:a@b", "1 twitter:x", "1 github:", "one github:x", "1"} {
		if _, err := parseTiers(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func tierServer(t *testing.T, tiers string) *Server {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tiers")
	if err := os.WriteFile(path, []byte(tiers), 0o600); err != nil {
		t.Fatal(err)
	}
	return New(Config{RobotToken: testToken, PairTTL: time.Minute, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ManagerURL: "https://sm.example", ManagerSecret: "x", ManagerSignIn: true, PublicURL: "https://chan.example",
		AdminEmails: []string{"admin@example.com"}, TiersFile: path, SponsorURL: "https://example.com/sponsor"})
}

func TestAccountTier(t *testing.T) {
	s := tierServer(t, "2 github:octocat\n3 email:friend@example.com\n")
	now := time.Now()
	for _, c := range []struct {
		a    Account
		want int
	}{
		{Account{Email: "admin@example.com"}, 1},
		{Account{Provider: "github", Login: "OctoCat"}, 2},
		{Account{Provider: "google", Login: "octocat"}, 4}, // a GitHub login only from GitHub
		{Account{Email: "friend@example.com", Provider: "google"}, 3},
		{Account{Email: "someone@example.com"}, 4},
		{Account{Email: "admin@example.com", Provider: "microsoft"}, 4}, // a tenant admin can set any e-mail
		{Account{Email: "friend@example.com", Provider: "microsoft"}, 4},
	} {
		if got := s.accountTier(c.a, now); got != c.want {
			t.Errorf("%+v: tier %d, want %d", c.a, got, c.want)
		}
	}

	// Signed in or not.
	s.logins["signed-in"] = Account{Email: "someone@example.com"}
	if s.sessionTier("anonymous", now) != 5 || s.sessionTier("signed-in", now) != 4 {
		t.Error("session tiers: want 5 anonymous, 4 signed in")
	}
	// Over the limit, the answer says how to get more.
	for session, hint := range map[string]string{"anonymous": "Sign in", "signed-in": "https://example.com/sponsor"} {
		rec := httptest.NewRecorder()
		s.tooMany(rec, session, "too many commands")
		if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), hint) {
			t.Errorf("%s: %d %q, want 429 with %q", session, rec.Code, rec.Body.String(), hint)
		}
	}
}

// The file is read again when it changes.
func TestTiersReload(t *testing.T) {
	s := tierServer(t, "3 github:octocat\n")
	a := Account{Provider: "github", Login: "octocat"}
	now := time.Now()
	if got := s.accountTier(a, now); got != 3 {
		t.Fatalf("tier %d, want 3", got)
	}
	if err := os.WriteFile(s.cfg.TiersFile, []byte("2 github:octocat\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(s.cfg.TiersFile, now.Add(time.Minute), now.Add(time.Minute))
	if got := s.accountTier(a, now.Add(tiersRecheck)); got != 2 {
		t.Fatalf("after the change: tier %d, want 2", got)
	}
	// A broken file keeps the rules from before.
	os.WriteFile(s.cfg.TiersFile, []byte("9 nobody\n"), 0o600)
	os.Chtimes(s.cfg.TiersFile, now.Add(2*time.Minute), now.Add(2*time.Minute))
	if got := s.accountTier(a, now.Add(2*tiersRecheck)); got != 2 {
		t.Fatalf("after a broken file: tier %d, want 2", got)
	}
}
