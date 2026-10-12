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

	"github.com/gorilla/websocket"
	"github.com/i41-eu/q41eu-raw/wire"
)

// writeTokens writes a tokens file and moves its modification time forward, so a rewrite
// within the same second is still seen as a change.
func writeTokens(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	next := time.Now().Add(time.Duration(len(lines)+1) * time.Second)
	if err := os.Chtimes(path, next, next); err != nil {
		t.Fatal(err)
	}
}

func newInviteServer(t *testing.T, tokensFile string) *httptest.Server {
	t.Helper()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(Config{RobotToken: testToken, RobotTokensFile: tokensFile, PairTTL: time.Minute, Log: quiet})
	ts := httptest.NewServer(s.Handler())
	s.cfg.PublicURL = ts.URL
	t.Cleanup(ts.Close)
	return ts
}

// registerGuest connects with a token and returns the frame kinds up to the PairCode.
func registerGuest(t *testing.T, ts *httptest.Server, token, id string) []string {
	t.Helper()
	ws, resp, err := dialRobot(ts, token, id)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("dial %s: %v (HTTP %d)", id, err, code)
	}
	t.Cleanup(func() { ws.Close() })
	r := &testRobot{t: t, ws: ws}
	r.send(wire.KindRegister, wire.RegisterBody{Class: wire.ClassRobot,
		Capabilities: wire.RobotCapabilities{Model: "test", Commands: []string{"nod"}}})
	var kinds []string
	ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		msgType, data, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("reading: %v (got %v)", err, kinds)
		}
		if msgType != websocket.TextMessage {
			continue
		}
		frames, err := wire.Parse(data)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range frames {
			kinds = append(kinds, f.Kind)
			if f.Kind == wire.KindPairCode {
				return kinds
			}
		}
	}
}

func dialStatus(ts *httptest.Server, token, id string) int {
	ws, resp, err := dialRobot(ts, token, id)
	if err == nil {
		ws.Close()
		return http.StatusSwitchingProtocols
	}
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

func TestInviteTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "robot-tokens")
	tok1, line1, err := NewRobotInvite("guest-1")
	if err != nil {
		t.Fatal(err)
	}
	tok2, line2, err := NewRobotInvite("guest-2")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(line1, tok1) {
		t.Fatal("the file line must not contain the token")
	}
	writeTokens(t, path, "# invited robots", line1)
	ts := newInviteServer(t, path)

	// A guest with its own token registers; the owner's shared token still works.
	kinds := registerGuest(t, ts, tok1, "guest-1")
	if kinds[0] != wire.KindAccepted {
		t.Fatalf("frames: %v", kinds)
	}
	if owner := registerGuest(t, ts, testToken, "own-1"); owner[0] != wire.KindAccepted {
		t.Fatalf("owner frames: %v", owner)
	}

	// A token works only for its own robot id; wrong tokens are refused.
	for _, c := range []struct{ token, id string }{
		{tok1, "guest-2"}, {tok2, "guest-2"}, {"wrong", "guest-1"}, {"", "guest-1"},
	} {
		if got := dialStatus(ts, c.token, c.id); got != http.StatusUnauthorized {
			t.Fatalf("token for %s as %s: HTTP %d, want 401", c.token[:min(4, len(c.token))], c.id, got)
		}
	}

	// Adding and revoking robots needs no restart.
	writeTokens(t, path, line2)
	registerGuest(t, ts, tok2, "guest-2")
	if got := dialStatus(ts, tok1, "guest-1"); got != http.StatusUnauthorized {
		t.Fatalf("revoked guest-1: HTTP %d, want 401", got)
	}

	// A broken file lets no guest in, and the owner's robots keep working.
	writeTokens(t, path, line2, "not a valid line")
	if got := dialStatus(ts, tok2, "guest-2"); got != http.StatusUnauthorized {
		t.Fatalf("guest with a broken file: HTTP %d, want 401", got)
	}
	registerGuest(t, ts, testToken, "own-2")
}

func TestInviteTokensFileMissing(t *testing.T) {
	ts := newInviteServer(t, filepath.Join(t.TempDir(), "absent"))
	if got := dialStatus(ts, "x", "guest-1"); got != http.StatusUnauthorized {
		t.Fatalf("HTTP %d, want 401", got)
	}
	registerGuest(t, ts, testToken, "own-1")
}

func TestReadRobotTokensErrors(t *testing.T) {
	_, good, _ := NewRobotInvite("r1")
	hash := strings.Fields(good)[1]
	for name, content := range map[string]string{
		"one field":  "r1\n",
		"bad id":     "bad/id " + hash + "\n",
		"short hash": "r1 abcd\n",
		"duplicate":  good + "\n" + good + "\n",
	} {
		path := filepath.Join(t.TempDir(), "f")
		os.WriteFile(path, []byte(content), 0o600)
		if _, err := readRobotTokens(path); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	path := filepath.Join(t.TempDir(), "f")
	os.WriteFile(path, []byte("# comment\n\n  "+good+"  \n"), 0o600)
	if h, err := readRobotTokens(path); err != nil || len(h) != 1 {
		t.Fatalf("good file: %v %v", h, err)
	}
	if _, _, err := NewRobotInvite("bad id"); err == nil {
		t.Fatal("NewRobotInvite accepted a bad id")
	}
}
