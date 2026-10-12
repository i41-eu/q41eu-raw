package statestore

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFile(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "sub", "state.json")
	s, err := Open(ctx, p, "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := s.Load(ctx); b != nil || err != nil {
		t.Fatalf("empty: %q %v", b, err)
	}
	if err := s.Save(ctx, []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.Load(ctx); string(b) != `{"a":1}` {
		t.Fatalf("load: %s", b)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", fi.Mode().Perm())
	}
	if s.Lost() != nil {
		t.Fatal("a file is never lost")
	}
	fb := &File{Path: p, BlobDir: filepath.Join(filepath.Dir(p), "photos")}
	if b, err := fb.Blob(ctx, "none.jpg"); b != nil || err != nil {
		t.Fatalf("no blob: %v %v", b, err)
	}
	fb.PutBlob(ctx, "a.jpg", []byte("x"))
	if names, _ := fb.BlobNames(ctx); len(names) != 1 {
		t.Fatalf("blob names: %v", names)
	}
}

// The Postgres tests need a database: STATESTORE_TEST_PG=postgres://postgres:test@127.0.0.1:55432/test
// (a throwaway container).
func testURL(t *testing.T) string {
	u := os.Getenv("STATESTORE_TEST_PG")
	if u == "" {
		t.Skip("STATESTORE_TEST_PG not set")
	}
	return u
}

func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return string(xa) == string(ya)
}

func TestPostgres(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	app := "test-" + strings.ReplaceAll(t.Name(), "/", "-") + time.Now().Format("150405.000000")

	// The move from a volume: the file's state goes in on the first open.
	dir := t.TempDir()
	f := &File{Path: filepath.Join(dir, "state.json"), BlobDir: filepath.Join(dir, "photos")}
	f.Save(ctx, []byte(`{"robots":{"r1":{"name":"Ema's robot"}},"n":1}`))
	f.PutBlob(ctx, "r1-1.jpg", []byte{0xff, 0xd8, 1, 2})
	s, err := Open(ctx, url, app, f)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := s.Load(ctx); err != nil || !sameJSON(t, b, []byte(`{"robots":{"r1":{"name":"Ema's robot"}},"n":1}`)) {
		t.Fatalf("imported: %s %v", b, err)
	}
	if b, err := s.Blob(ctx, "r1-1.jpg"); err != nil || string(b) != string([]byte{0xff, 0xd8, 1, 2}) {
		t.Fatalf("imported blob: %v %v", b, err)
	}
	if err := s.PutBlob(ctx, "../x", nil); err == nil {
		t.Fatal("a blob name with a path")
	}
	s.PutBlob(ctx, "r1-2.jpg", []byte{9})
	s.DeleteBlob(ctx, "r1-1.jpg")
	if names, _ := s.BlobNames(ctx); len(names) != 1 || names[0] != "r1-2.jpg" {
		t.Fatalf("blobs: %v", names)
	}
	for i := 2; i <= Keep+5; i++ {
		if err := s.Save(ctx, []byte(`{"n":`+itoa(i)+`}`)); err != nil {
			t.Fatal(err)
		}
	}
	if b, _ := s.Load(ctx); !sameJSON(t, b, []byte(`{"n":55}`)) {
		t.Fatalf("after saves: %s", b)
	}
	pg := s.(*Postgres)
	var kept, oldest int64
	pg.mu.Lock()
	pg.conn.QueryRow(ctx, "SELECT count(*), min(version) FROM state_history WHERE app = $1", app).Scan(&kept, &oldest)
	pg.mu.Unlock()
	if kept != Keep || oldest != 55-Keep+1 {
		t.Fatalf("history: %d kept, oldest %d", kept, oldest)
	}

	// A second copy waits for the lock until the first closes.
	got := make(chan error, 1)
	go func() {
		s2, err := Open(ctx, url, app, f) // the import does not run again: the row exists
		if err == nil {
			b, _ := s2.Load(ctx)
			if !sameJSON(t, b, []byte(`{"n":55}`)) {
				err = errorString("second copy loaded " + string(b))
			}
			s2.Close()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("the second copy did not wait: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	s.Close()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second copy never got the lock")
	}
}

// A lost connection closes Lost, and saves stop.
func TestPostgresLost(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	PingEvery = 100 * time.Millisecond
	defer func() { PingEvery = 10 * time.Second }()
	app := "test-lost-" + time.Now().Format("150405.000000")
	s, err := Open(ctx, url, app, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	pg := s.(*Postgres)
	// Another session kills ours (as a restart of the server would).
	other, err := Open(ctx, url, app+"-killer", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	pid := pg.pid()
	op := other.(*Postgres)
	op.mu.Lock()
	_, err = op.conn.Exec(ctx, "SELECT pg_terminate_backend($1)", pid)
	op.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Lost():
	case <-time.After(3 * time.Second):
		t.Fatal("Lost not closed")
	}
	if err := s.Save(ctx, []byte(`{}`)); err == nil {
		t.Fatal("saved after the lock was lost")
	}
}

func TestRedact(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second) // Open retries until then
	defer cancel()
	_, err := Open(ctx, "postgres://u:sekrit-pass@127.0.0.1:1/db?connect_timeout=1", "x", nil)
	if err == nil || strings.Contains(err.Error(), "sekrit-pass") {
		t.Fatalf("error: %v", err)
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }

func itoa(i int) string { return strconv.Itoa(i) }

// Nothing listening: Open keeps trying until its context ends, instead of failing at once.
func TestConnectRetries(t *testing.T) {
	ConnectRetryFirst, ConnectRetryMax = 20*time.Millisecond, 50*time.Millisecond
	defer func() { ConnectRetryFirst, ConnectRetryMax = time.Second, 10*time.Second }()
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Open(ctx, "postgres://u:sekrit-pass@127.0.0.1:1/db?connect_timeout=1", "x", nil)
	if err == nil || strings.Contains(err.Error(), "sekrit-pass") {
		t.Fatalf("error: %v", err)
	}
	if d := time.Since(start); d < 350*time.Millisecond {
		t.Fatalf("gave up after %v, before its context ended", d)
	}
}
