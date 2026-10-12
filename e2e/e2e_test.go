package e2e

import (
	"bytes"
	"crypto/ecdh"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/vectors.json")

func fixedKey(t *testing.T, fill byte) *ecdh.PrivateKey {
	k, err := ecdh.X25519().NewPrivateKey(bytes.Repeat([]byte{fill}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestEnrollAndPairwise(t *testing.T) {
	r, _ := NewKey()
	b, _ := NewKey()
	p := NewSecret()

	// The browser reads R_pub and P from the fragment.
	rPub, gotP, err := ParseFragment("#" + Fragment(r.PublicKey(), p))
	if err != nil || !bytes.Equal(rPub.Bytes(), r.PublicKey().Bytes()) || !bytes.Equal(gotP, p) {
		t.Fatalf("fragment round trip: %v", err)
	}
	mac := b64.EncodeToString(EnrollMAC(p, rPub, b.PublicKey()))
	if err := CheckEnroll([][]byte{NewSecret(), p}, r.PublicKey(), b.PublicKey(), mac); err != nil {
		t.Fatal("a valid enrollment was refused")
	}
	// A relay without P cannot enroll its own key.
	evil, _ := NewKey()
	forged := b64.EncodeToString(EnrollMAC(NewSecret(), r.PublicKey(), evil.PublicKey()))
	if CheckEnroll([][]byte{p}, r.PublicKey(), evil.PublicKey(), forged) == nil {
		t.Fatal("a forged enrollment was accepted")
	}
	// A valid MAC for B does not enroll another key.
	if CheckEnroll([][]byte{p}, r.PublicKey(), evil.PublicKey(), mac) == nil {
		t.Fatal("a MAC was accepted for another key")
	}

	kr, _ := Pairwise(r, b.PublicKey(), r.PublicKey(), b.PublicKey())
	kb, _ := Pairwise(b, r.PublicKey(), r.PublicKey(), b.PublicKey())
	if !bytes.Equal(kr, kb) || len(kr) != KeySize {
		t.Fatal("robot and browser derive different pairwise keys")
	}
}

func TestBinaryAndJSON(t *testing.T) {
	g := NewSecret()
	g = append(g, g...) // 32 bytes
	ns := NewNonces()
	msg, err := SealBinary(g, 7, ns.Next(), "stackchan-1", 0x01, []byte("jpeg frame"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(msg, []byte("jpeg frame")) {
		t.Fatal("plaintext visible in a sealed message")
	}
	keys := func(e uint32) []byte {
		if e == 7 {
			return g
		}
		return nil
	}
	inner, payload, err := OpenBinary(keys, "stackchan-1", msg)
	if err != nil || inner != 0x01 || string(payload) != "jpeg frame" {
		t.Fatalf("open: %v %x %q", err, inner, payload)
	}
	if _, _, err := OpenBinary(keys, "stackchan-2", msg); err == nil {
		t.Fatal("a message opened under another robot id (aad)")
	}
	msg[len(msg)-1] ^= 1
	if _, _, err := OpenBinary(keys, "stackchan-1", msg); err == nil {
		t.Fatal("a tampered message opened")
	}
	if a, b := ns.Next(), ns.Next(); bytes.Equal(a, b) {
		t.Fatal("nonces repeat")
	}

	s, _ := SealJSON(g, ns.Next(), []byte(`{"command":"nod"}`), []byte("stackchan-1"))
	plain, err := OpenJSON(g, s, []byte("stackchan-1"))
	if err != nil || string(plain) != `{"command":"nod"}` {
		t.Fatalf("json: %v %s", err, plain)
	}

	seqs := NewSeqs()
	if seqs.Accept("b1", 1) != nil || seqs.Accept("b1", 1) == nil || seqs.Accept("b1", 2) != nil || seqs.Accept("b2", 1) != nil {
		t.Fatal("sequence checks")
	}
}

// vectors are fixed inputs and their outputs, for the firmware and the browser to check
// their implementations against (testdata/vectors.json).
type vectors struct {
	RobotPriv, RobotPub, BrowserPriv, BrowserPub string // hex / base64url keys
	Secret, Fragment, EnrollMAC, BrowserID       string
	Pairwise                                     string // hex
	RobotID                                      string
	Epoch                                        uint32
	GroupKey                                     string // hex
	GroupKeyNonce, GroupKeySealed                string // the E2EGroupKey body
	BinaryNonce                                  string // hex
	BinaryInner                                  byte
	BinaryPayload, BinarySealed                  string // hex
	CommandNonce, CommandPlain, CommandSealed    string
}

func TestVectors(t *testing.T) {
	r, b := fixedKey(t, 0x11), fixedKey(t, 0x22)
	p := bytes.Repeat([]byte{0x33}, SecretSize)
	g := bytes.Repeat([]byte{0x44}, KeySize)
	robotID := "stackchan-0a1b2c3d4e50"
	k, err := Pairwise(r, b.PublicKey(), r.PublicKey(), b.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	gkNonce := bytes.Repeat([]byte{0x55}, NonceSize)
	gk, _ := SealJSON(k, gkNonce, g, GroupAAD(robotID, 3))
	binNonce := bytes.Repeat([]byte{0x66}, NonceSize)
	bin, _ := SealBinary(g, 3, binNonce, robotID, 0x01, []byte("frame"))
	cmdNonce := bytes.Repeat([]byte{0x77}, NonceSize)
	cmdPlain := `{"command":"nod","args":{},"seq":1}`
	cmd, _ := SealJSON(k, cmdNonce, []byte(cmdPlain), []byte(robotID))

	got := vectors{
		RobotPriv: hex.EncodeToString(r.Bytes()), RobotPub: EncodePublic(r.PublicKey()),
		BrowserPriv: hex.EncodeToString(b.Bytes()), BrowserPub: EncodePublic(b.PublicKey()),
		Secret: b64.EncodeToString(p), Fragment: Fragment(r.PublicKey(), p),
		EnrollMAC: b64.EncodeToString(EnrollMAC(p, r.PublicKey(), b.PublicKey())), BrowserID: BrowserID(b.PublicKey()),
		Pairwise: hex.EncodeToString(k), RobotID: robotID, Epoch: 3, GroupKey: hex.EncodeToString(g),
		GroupKeyNonce: gk.N, GroupKeySealed: gk.C,
		BinaryNonce: hex.EncodeToString(binNonce), BinaryInner: 0x01,
		BinaryPayload: hex.EncodeToString([]byte("frame")), BinarySealed: hex.EncodeToString(bin),
		CommandNonce: cmd.N, CommandPlain: cmdPlain, CommandSealed: cmd.C,
	}
	raw, _ := json.MarshalIndent(got, "", "  ")
	const path = "testdata/vectors.json"
	if *update {
		os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./e2e -update once)", err)
	}
	if !bytes.Equal(bytes.TrimSpace(want), bytes.TrimSpace(raw)) {
		t.Fatalf("vectors changed; if on purpose, rerun with -update and update the firmware and browser")
	}
}
