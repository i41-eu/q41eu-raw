package main

// End-to-end encryption on the robot side (i41eu-arch docs/e2ee.md), as the firmware will
// do it: with -e2e, the fake robot seals telemetry, events and media under its group key,
// accepts commands only sealed by an enrolled browser (except the relay's camera and mic
// switches), and prints its pairing URL with the QR fragment.

import (
	"crypto/ecdh"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"sync"

	"github.com/i41-eu/q41eu-raw/e2e"
	"github.com/i41-eu/q41eu-raw/wire"
)

// relaySwitches are the plaintext commands an encrypted robot still takes from the relay:
// they switch streams, and what streams stays sealed.
var relaySwitches = map[string]bool{"camera": true, "mic": true, "imu_stream": true, "touch_stream": true, "light_stream": true}

type e2eRobot struct {
	id    string
	path  string // state file: the robot key and the enrolled browsers
	log   *slog.Logger
	key   *ecdh.PrivateKey
	group []byte
	epoch uint32

	mu       sync.Mutex
	pairURL  string                     // the pairing URL the server sent last (without fragment)
	secrets  [][]byte                   // current and previous pairing secret
	enrolled map[string]*ecdh.PublicKey // browser id -> key
	pairwise map[string][]byte          // browser id -> K_B
	nonces   *e2e.Nonces
	seqs     *e2e.Seqs
}

type e2eFile struct {
	Key      string   `json:"robot_key"` // hex X25519 private key
	Browsers []string `json:"browsers"`  // base64url public keys
	Epoch    uint32   `json:"epoch"`
}

func newE2ERobot(id, path string, log *slog.Logger) (*e2eRobot, error) {
	r := &e2eRobot{id: id, path: path, log: log, enrolled: map[string]*ecdh.PublicKey{},
		pairwise: map[string][]byte{}, nonces: e2e.NewNonces(), seqs: e2e.NewSeqs()}
	var st e2eFile
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &st); err != nil {
			return nil, err
		}
	}
	if raw, err := hex.DecodeString(st.Key); err == nil && len(raw) == e2e.KeySize {
		if r.key, err = ecdh.X25519().NewPrivateKey(raw); err != nil {
			return nil, err
		}
	} else {
		var err error
		if r.key, err = e2e.NewKey(); err != nil {
			return nil, err
		}
	}
	for _, b := range st.Browsers {
		if pub, err := e2e.ParsePublic(b); err == nil {
			r.enroll(pub)
		}
	}
	r.epoch = st.Epoch + 1 // a new group key on every start
	r.group = append(e2e.NewSecret(), e2e.NewSecret()...)
	r.secrets = [][]byte{e2e.NewSecret()}
	return r, r.save()
}

func (r *e2eRobot) save() error {
	st := e2eFile{Key: hex.EncodeToString(r.key.Bytes()), Epoch: r.epoch}
	for _, pub := range r.enrolled {
		st.Browsers = append(st.Browsers, e2e.EncodePublic(pub))
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	return os.WriteFile(r.path, b, 0o600)
}

// enroll must hold r.mu (or run before any goroutine starts).
func (r *e2eRobot) enroll(b *ecdh.PublicKey) string {
	id := e2e.BrowserID(b)
	k, _ := e2e.Pairwise(r.key, b, r.key.PublicKey(), b)
	r.enrolled[id], r.pairwise[id] = b, k
	return id
}

// qrURL is what the robot shows as its QR code for a pairing URL from the server: the URL
// with a fresh pairing secret in the fragment. The previous secret stays valid (a QR code
// that changed while someone scanned it).
func (r *e2eRobot) qrURL(pairURL string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pairURL = pairURL
	p := e2e.NewSecret()
	r.secrets = append([][]byte{p}, r.secrets[:min(len(r.secrets), 1)]...)
	return pairURL + "#" + e2e.Fragment(r.key.PublicKey(), p)
}

// handleEnroll checks a browser's MAC and enrolls it; the answer is its group key.
func (r *e2eRobot) handleEnroll(body json.RawMessage) (wire.Frame, error) {
	var req wire.E2EEnrollBody
	if err := json.Unmarshal(body, &req); err != nil {
		return wire.Frame{}, err
	}
	b, err := e2e.ParsePublic(req.B)
	if err != nil {
		return wire.Frame{}, err
	}
	r.mu.Lock()
	i, err := e2e.MatchEnroll(r.secrets, r.key.PublicKey(), b, req.MAC)
	var id, pairURL string
	if err == nil {
		id = r.enroll(b)
		r.secrets = append(r.secrets[:i:i], r.secrets[i+1:]...) // one secret enrolls one browser
		pairURL = r.pairURL
	}
	r.mu.Unlock()
	if err != nil {
		return wire.Frame{}, err
	}
	r.save()
	r.log.Info("browser enrolled (e2e)", "browser", id)
	if pairURL != "" { // the robot redraws its QR code with a fresh secret
		r.log.Info("pair by opening this URL (the robot would show it as a QR)", "url", r.qrURL(pairURL))
	}
	return r.groupKeyFor(id)
}

// handleHello answers a returning browser with the current group key.
func (r *e2eRobot) handleHello(body json.RawMessage) (wire.Frame, error) {
	var req wire.E2EHelloBody
	if err := json.Unmarshal(body, &req); err != nil {
		return wire.Frame{}, err
	}
	return r.groupKeyFor(req.B)
}

func (r *e2eRobot) groupKeyFor(browser string) (wire.Frame, error) {
	r.mu.Lock()
	k := r.pairwise[browser]
	r.mu.Unlock()
	if k == nil {
		return wire.Frame{}, errors.New("browser not enrolled")
	}
	s, err := e2e.SealJSON(k, r.nonces.Next(), r.group, e2e.GroupAAD(r.id, r.epoch))
	if err != nil {
		return wire.Frame{}, err
	}
	s.B, s.Epoch = browser, r.epoch
	raw, _ := json.Marshal(s)
	return wire.Frame{Kind: wire.KindE2EGroupKey, Body: raw}, nil
}

// openCommand opens a sealed command from an enrolled browser, refusing replays.
func (r *e2eRobot) openCommand(body json.RawMessage) (wire.RobotCommandBody, error) {
	var s e2e.Sealed
	if err := json.Unmarshal(body, &s); err != nil {
		return wire.RobotCommandBody{}, err
	}
	r.mu.Lock()
	k := r.pairwise[s.B]
	r.mu.Unlock()
	if k == nil {
		return wire.RobotCommandBody{}, errors.New("browser not enrolled")
	}
	plain, err := e2e.OpenJSON(k, s, []byte(r.id))
	if err != nil {
		return wire.RobotCommandBody{}, err
	}
	var cmd struct {
		wire.RobotCommandBody
		Seq uint64 `json:"seq"`
	}
	if err := json.Unmarshal(plain, &cmd); err != nil {
		return wire.RobotCommandBody{}, err
	}
	if err := r.seqs.Accept(s.B, cmd.Seq); err != nil {
		return wire.RobotCommandBody{}, err
	}
	return cmd.RobotCommandBody, nil
}

// sealFrame wraps a frame the robot would send in plaintext as E2EData.
func (r *e2eRobot) sealFrame(kind string, body any) (e2e.Sealed, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return e2e.Sealed{}, err
	}
	plain, _ := json.Marshal(map[string]any{"kind": kind, "body": json.RawMessage(b)})
	s, err := e2e.SealJSON(r.group, r.nonces.Next(), plain, []byte(r.id))
	s.Epoch = r.epoch
	return s, err
}

// sealBinary turns a plaintext binary message (type byte + payload) into a 0x30 message.
func (r *e2eRobot) sealBinary(msg []byte) ([]byte, error) {
	return e2e.SealBinary(r.group, r.epoch, r.nonces.Next(), r.id, msg[0], msg[1:])
}
