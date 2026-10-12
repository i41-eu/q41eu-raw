// Package e2e is the reference implementation of end-to-end encryption between a robot
// and the browsers its owner enrolled, so a relay carries only ciphertext. The design and
// its threat model are in the i41eu-arch repo, docs/e2ee.md; this package follows it, and
// its test vectors (testdata/vectors.json) let the firmware and the browser check theirs.
//
// Primitives: X25519, HKDF-SHA256, HMAC-SHA256, AES-256-GCM with 96-bit nonces.
package e2e

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
)

const (
	KeySize        = 32
	SecretSize     = 16 // the pairing secret P
	NonceSize      = 12
	BrowserIDBytes = 8

	// Binary message types (wire protocol §6).
	BinGroup   byte = 0x30 // robot -> browsers: epoch u32 | nonce | AES-GCM(G, inner type + payload)
	BinBrowser byte = 0x31 // browser -> robot: browser id (8) | nonce | AES-GCM(K_B, inner type + payload)

	fragmentPrefix = "e2e=1."
	enrollLabel    = "i41-e2e-enroll|"
	pairwiseInfo   = "i41-e2e pairwise"
	groupLabel     = "i41-e2e group|"
)

var (
	ErrBadMAC   = errors.New("e2e: enrollment MAC does not match")
	ErrReplay   = errors.New("e2e: command sequence not newer than the last one")
	ErrTooShort = errors.New("e2e: message too short")
)

var b64 = base64.RawURLEncoding

// NewKey makes an X25519 key pair (the robot's R or a browser's B).
func NewKey() (*ecdh.PrivateKey, error) { return ecdh.X25519().GenerateKey(rand.Reader) }

// ParsePublic reads a base64url X25519 public key.
func ParsePublic(s string) (*ecdh.PublicKey, error) {
	b, err := b64.DecodeString(s)
	if err != nil || len(b) != KeySize {
		return nil, fmt.Errorf("e2e: bad public key")
	}
	return ecdh.X25519().NewPublicKey(b)
}

// EncodePublic is the base64url form used on the wire and in the QR fragment.
func EncodePublic(k *ecdh.PublicKey) string { return b64.EncodeToString(k.Bytes()) }

// BrowserID is the short id of a browser key: the first 8 bytes of SHA-256(B_pub), hex.
func BrowserID(b *ecdh.PublicKey) string {
	sum := sha256.Sum256(b.Bytes())
	return hex.EncodeToString(sum[:BrowserIDBytes])
}

// NewSecret makes a pairing secret P.
func NewSecret() []byte {
	p := make([]byte, SecretSize)
	rand.Read(p)
	return p
}

// Fragment is what the robot appends to its pairing URL: "#e2e=1.<R_pub>.<P>" without "#".
func Fragment(r *ecdh.PublicKey, p []byte) string {
	return fragmentPrefix + EncodePublic(r) + "." + b64.EncodeToString(p)
}

// ParseFragment reads a pairing URL fragment (with or without the leading "#").
func ParseFragment(f string) (*ecdh.PublicKey, []byte, error) {
	rest, ok := strings.CutPrefix(strings.TrimPrefix(f, "#"), fragmentPrefix)
	if !ok {
		return nil, nil, errors.New("e2e: no e2e fragment")
	}
	rs, ps, ok := strings.Cut(rest, ".")
	if !ok {
		return nil, nil, errors.New("e2e: bad fragment")
	}
	r, err := ParsePublic(rs)
	if err != nil {
		return nil, nil, err
	}
	p, err := b64.DecodeString(ps)
	if err != nil || len(p) != SecretSize {
		return nil, nil, errors.New("e2e: bad pairing secret")
	}
	return r, p, nil
}

// EnrollMAC proves the browser saw the QR code: HMAC-SHA256(P, label | R_pub | B_pub),
// with both keys in base64url.
func EnrollMAC(p []byte, r, b *ecdh.PublicKey) []byte {
	m := hmac.New(sha256.New, p)
	m.Write([]byte(enrollLabel + EncodePublic(r) + "|" + EncodePublic(b)))
	return m.Sum(nil)
}

// CheckEnroll verifies an enrollment MAC (base64url) against any of the pairing secrets.
func CheckEnroll(secrets [][]byte, r, b *ecdh.PublicKey, mac string) error {
	_, err := MatchEnroll(secrets, r, b, mac)
	return err
}

// MatchEnroll is CheckEnroll that also tells which secret matched, so the robot can consume
// exactly that one (each secret enrolls one browser) and keep the others valid.
func MatchEnroll(secrets [][]byte, r, b *ecdh.PublicKey, mac string) (int, error) {
	got, err := b64.DecodeString(mac)
	if err != nil {
		return -1, ErrBadMAC
	}
	for i, p := range secrets {
		if p != nil && hmac.Equal(got, EnrollMAC(p, r, b)) {
			return i, nil
		}
	}
	return -1, ErrBadMAC
}

// Pairwise derives K_B from one side's private key and the other side's public key:
// HKDF-SHA256(ikm = X25519, salt = R_pub || B_pub, info = "i41-e2e pairwise").
func Pairwise(own *ecdh.PrivateKey, peer *ecdh.PublicKey, r, b *ecdh.PublicKey) ([]byte, error) {
	shared, err := own.ECDH(peer)
	if err != nil {
		return nil, err
	}
	salt := append(append([]byte{}, r.Bytes()...), b.Bytes()...)
	return hkdf.Key(sha256.New, shared, salt, pairwiseInfo, KeySize)
}

func gcm(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal encrypts with AES-256-GCM.
func Seal(key, nonce, plain, aad []byte) ([]byte, error) {
	a, err := gcm(key)
	if err != nil {
		return nil, err
	}
	return a.Seal(nil, nonce, plain, aad), nil
}

// Open decrypts with AES-256-GCM.
func Open(key, nonce, sealed, aad []byte) ([]byte, error) {
	a, err := gcm(key)
	if err != nil {
		return nil, err
	}
	return a.Open(nil, nonce, sealed, aad)
}

// GroupAAD is the associated data for a sealed group key.
func GroupAAD(robotID string, epoch uint32) []byte {
	return fmt.Appendf(nil, "%s%s|%d", groupLabel, robotID, epoch)
}

// Nonces gives nonces that never repeat under one key: a random 4-byte prefix chosen
// once, then a 64-bit counter.
type Nonces struct {
	mu     sync.Mutex
	prefix [4]byte
	n      uint64
}

func NewNonces() *Nonces {
	ns := &Nonces{}
	rand.Read(ns.prefix[:])
	return ns
}

func (ns *Nonces) Next() []byte {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	ns.n++
	out := make([]byte, NonceSize)
	copy(out, ns.prefix[:])
	binary.BigEndian.PutUint64(out[4:], ns.n)
	return out
}

// SealBinary makes a 0x30 message: epoch | nonce | AES-GCM(G, inner type + payload), aad = robot id.
func SealBinary(g []byte, epoch uint32, nonce []byte, robotID string, inner byte, payload []byte) ([]byte, error) {
	plain := append([]byte{inner}, payload...)
	c, err := Seal(g, nonce, plain, []byte(robotID))
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 1+4+NonceSize+len(c))
	out = append(out, BinGroup)
	out = binary.BigEndian.AppendUint32(out, epoch)
	out = append(out, nonce...)
	return append(out, c...), nil
}

// OpenBinary reads a 0x30 message with the group key of its epoch (keys by epoch).
func OpenBinary(keys func(epoch uint32) []byte, robotID string, msg []byte) (inner byte, payload []byte, err error) {
	if len(msg) < 1+4+NonceSize+16 || msg[0] != BinGroup {
		return 0, nil, ErrTooShort
	}
	epoch := binary.BigEndian.Uint32(msg[1:5])
	g := keys(epoch)
	if g == nil {
		return 0, nil, fmt.Errorf("e2e: no group key for epoch %d", epoch)
	}
	plain, err := Open(g, msg[5:5+NonceSize], msg[5+NonceSize:], []byte(robotID))
	if err != nil {
		return 0, nil, err
	}
	if len(plain) == 0 {
		return 0, nil, ErrTooShort
	}
	return plain[0], plain[1:], nil
}

// Sealed is the JSON body of E2EGroupKey, E2EData and E2ECommand: nonce and ciphertext in
// base64url, plus who (b) and which key (epoch) where it applies.
type Sealed struct {
	B     string `json:"b,omitempty"`
	Epoch uint32 `json:"epoch,omitempty"`
	N     string `json:"n"`
	C     string `json:"c"`
}

// SealJSON seals plain into a Sealed body.
func SealJSON(key, nonce, plain, aad []byte) (Sealed, error) {
	c, err := Seal(key, nonce, plain, aad)
	if err != nil {
		return Sealed{}, err
	}
	return Sealed{N: b64.EncodeToString(nonce), C: b64.EncodeToString(c)}, nil
}

// OpenJSON opens a Sealed body.
func OpenJSON(key []byte, s Sealed, aad []byte) ([]byte, error) {
	n, err := b64.DecodeString(s.N)
	if err != nil || len(n) != NonceSize {
		return nil, errors.New("e2e: bad nonce")
	}
	c, err := b64.DecodeString(s.C)
	if err != nil {
		return nil, errors.New("e2e: bad ciphertext")
	}
	return Open(key, n, c, aad)
}

// Seqs keeps the last accepted command sequence per browser (replay protection).
type Seqs struct {
	mu   sync.Mutex
	last map[string]uint64
}

func NewSeqs() *Seqs { return &Seqs{last: map[string]uint64{}} }

// Accept records seq for browser b if it is newer than the last one.
func (s *Seqs) Accept(b string, seq uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if seq <= s.last[b] {
		return ErrReplay
	}
	s.last[b] = seq
	return nil
}
