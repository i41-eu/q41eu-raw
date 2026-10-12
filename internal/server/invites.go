package server

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Per-robot invite tokens (-robot-tokens-file): lets other people's robots use this
// server without the shared robot token. Each line binds one robot id to the SHA-256 of
// its own token, so a robot cannot connect as another one, and the file holds no token:
//
//	# comment
//	stackchan-0a1b2c3d4e50 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08
//
// The file is read again when it changes, so adding or revoking a robot needs no restart.
// Robots with an invite token are guests.

// robotInvites holds the parsed tokens file.
type robotInvites struct {
	path string

	mu      sync.Mutex
	modTime time.Time
	size    int64
	hashes  map[string][32]byte // robot id -> SHA-256 of its token
	err     error               // last load error, logged once per change
}

func newRobotInvites(path string) *robotInvites {
	return &robotInvites{path: path, hashes: map[string][32]byte{}}
}

// ok reports whether token is the invite token of robot id. It reloads the file first if
// it changed. A missing or broken file allows no invited robot.
func (ri *robotInvites) ok(id, token string) (bool, error) {
	if ri == nil || ri.path == "" || token == "" {
		return false, nil
	}
	ri.mu.Lock()
	defer ri.mu.Unlock()
	reloadErr := ri.reload()
	want, found := ri.hashes[id]
	got := sha256.Sum256([]byte(token))
	// Compare even when the id is unknown, so timing does not tell which ids are invited.
	match := subtle.ConstantTimeCompare(got[:], want[:]) == 1
	return found && match, reloadErr
}

// reload re-reads the file when its size or modification time changed. Must be called
// with ri.mu held. It returns an error only when the file changed and could not be read.
func (ri *robotInvites) reload() error {
	st, err := os.Stat(ri.path)
	if err != nil {
		if ri.err == nil || ri.err.Error() != err.Error() {
			ri.err = err
			ri.hashes = map[string][32]byte{}
			return err
		}
		return nil
	}
	if st.ModTime().Equal(ri.modTime) && st.Size() == ri.size && ri.err == nil {
		return nil
	}
	hashes, err := readRobotTokens(ri.path)
	same := err != nil && ri.err != nil && err.Error() == ri.err.Error()
	ri.modTime, ri.size, ri.err = st.ModTime(), st.Size(), err
	if err != nil {
		ri.hashes = map[string][32]byte{}
		if same {
			return nil // already reported
		}
		return err
	}
	ri.hashes = hashes
	return nil
}

// has reports whether the file lists robot id.
func (ri *robotInvites) has(id string) bool {
	ri.mu.Lock()
	defer ri.mu.Unlock()
	ri.reload()
	_, ok := ri.hashes[id]
	return ok
}

// count is the number of invited robots in the file as last read.
func (ri *robotInvites) count() int {
	ri.mu.Lock()
	defer ri.mu.Unlock()
	return len(ri.hashes)
}

func readRobotTokens(path string) (map[string][32]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	hashes := map[string][32]byte{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("%s:%d: want \"<robot id> <sha256 of its token>\"", path, n)
		}
		id, hexHash := fields[0], fields[1]
		if !robotIDPattern.MatchString(id) {
			return nil, fmt.Errorf("%s:%d: invalid robot id %q", path, n, id)
		}
		b, err := hex.DecodeString(hexHash)
		if err != nil || len(b) != sha256.Size {
			return nil, fmt.Errorf("%s:%d: the hash must be 64 hex characters (SHA-256)", path, n)
		}
		if _, dup := hashes[id]; dup {
			return nil, fmt.Errorf("%s:%d: robot %s is listed twice", path, n, id)
		}
		hashes[id] = [32]byte(b)
	}
	return hashes, sc.Err()
}

// NewRobotInvite makes a new random invite token for a robot. It returns the token (for
// the robot's sdkconfig) and the line for the tokens file (which holds only its hash).
func NewRobotInvite(robotID string) (token, line string, err error) {
	if !robotIDPattern.MatchString(robotID) {
		return "", "", fmt.Errorf("invalid robot id %q (1-64 of A-Z a-z 0-9 . _ -)", robotID)
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = hex.EncodeToString(b)
	sum := sha256.Sum256([]byte(token))
	return token, robotID + " " + hex.EncodeToString(sum[:]), nil
}
