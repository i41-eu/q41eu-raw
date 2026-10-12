package sso

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestSilentHint(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	req := func(hint, tried string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		if hint != "" {
			r.AddCookie(&http.Cookie{Name: HintCookie, Value: hint})
		}
		if tried != "" {
			r.AddCookie(&http.Cookie{Name: "app_sso_tried", Value: tried})
		}
		return r
	}
	at := func(d time.Duration) string { return strconv.FormatInt(now.Add(-d).Unix(), 10) }
	cases := []struct {
		name, hint, tried, want string
	}{
		{"no hint: never", "", "", ""},
		{"a hint, never tried: try", "h1", "", "h1"},
		{"tried with it a minute ago: not again", "h1", "h1|" + at(time.Minute), ""},
		{"tried with it long ago, still no session here: try again", "h1", "h1|" + at(RetryAfter+time.Second), "h1"},
		{"a new sign-in at the manager: try", "h2", "h1|" + at(time.Minute), "h2"},
		{"an old tried cookie without a time: try", "h1", "h1", "h1"},
	}
	for _, c := range cases {
		if got := silentHint(req(c.hint, c.tried), "app_sso_tried", now, false); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// TestSilentTry: an app on another domain than the manager's hint cookie tries without it too.
func TestSilentTry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	req := func(hint, tried string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		if hint != "" {
			r.AddCookie(&http.Cookie{Name: HintCookie, Value: hint})
		}
		if tried != "" {
			r.AddCookie(&http.Cookie{Name: "app_sso_tried", Value: tried})
		}
		return r
	}
	at := func(d time.Duration) string { return strconv.FormatInt(now.Add(-d).Unix(), 10) }
	cases := []struct {
		name, hint, tried, want string
	}{
		{"no hint, never tried: try", "", "", NoHint},
		{"no hint, tried a minute ago: not again", "", NoHint + "|" + at(time.Minute), ""},
		{"no hint, tried long ago: try again", "", NoHint + "|" + at(RetryAfter+time.Second), NoHint},
		{"a hint after all: try with it", "h1", NoHint + "|" + at(time.Minute), "h1"},
		{"a bad hint: as none", "a|b", "", NoHint},
	}
	for _, c := range cases {
		if got := silentHint(req(c.hint, c.tried), "app_sso_tried", now, true); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
