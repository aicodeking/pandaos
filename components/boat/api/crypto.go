package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Keyring holds subkeys derived from one 32-byte master key:
//   - enc:    AES-256-GCM key for secrets and webhook signing secrets at rest
//   - pepper: HMAC key for API-key hashes
//
// Key hashing: API-key secrets are 256 random bits, so a slow KDF adds nothing
// (brute force is infeasible either way). We store
// hex(HMAC-SHA256(pepper, salt || 0x00 || secret)) with a per-key 16-byte salt.
// A leaked state.json without the master key cannot be used to check guesses.
type Keyring struct {
	aead   cipher.AEAD
	pepper []byte
	aiKey  []byte // signs per-VM AI proxy tokens
	vmKey  []byte // signs per-VM API tokens (selftoken.go)
	// fileKey signs boat.dev download URLs (compat_download.go)
	fileKey []byte
}

func derive(master []byte, label string) []byte {
	m := hmac.New(sha256.New, master)
	m.Write([]byte(label))
	return m.Sum(nil)
}

func NewKeyring(master []byte) (*Keyring, error) {
	if len(master) != 32 {
		return nil, fmt.Errorf("master key must be 32 bytes, got %d", len(master))
	}
	block, err := aes.NewCipher(derive(master, "boat-api secrets v1"))
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Keyring{aead: aead, pepper: derive(master, "boat-api key pepper v1"), aiKey: derive(master, "boat-api ai proxy v1"),
		vmKey: derive(master, "boat-api vm token v1"), fileKey: derive(master, "boat-api file download v1")}, nil
}

// LoadMasterKey reads 32 raw bytes or 64 hex chars. With create, a missing
// file is generated with mode 0600 (systemd credentials are never created).
func LoadMasterKey(path string, create bool) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && create {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		if _, err := f.Write([]byte(hex.EncodeToString(key) + "\n")); err != nil {
			return nil, err
		}
		return key, f.Sync()
	}
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0o077 != 0 && !isCredential(path) {
		return nil, fmt.Errorf("%s must not be group/world readable (chmod 600)", path)
	}
	if len(b) == 32 {
		return b, nil
	}
	k, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(k) != 32 {
		return nil, fmt.Errorf("%s: want 32 raw bytes or 64 hex chars", path)
	}
	return k, nil
}

func (k *Keyring) Seal(plain []byte, aad string) (string, error) {
	nonce := make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := k.aead.Seal(nonce, nonce, plain, []byte(aad))
	return base64.StdEncoding.EncodeToString(ct), nil
}

func (k *Keyring) Open(sealed, aad string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil || len(b) < k.aead.NonceSize() {
		return nil, errors.New("malformed ciphertext")
	}
	n := k.aead.NonceSize()
	return k.aead.Open(nil, b[:n], b[n:], []byte(aad))
}

func (k *Keyring) HashSecret(salt, secret string) string {
	m := hmac.New(sha256.New, k.pepper)
	m.Write([]byte(salt))
	m.Write([]byte{0})
	m.Write([]byte(secret))
	return hex.EncodeToString(m.Sum(nil))
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b)
}

const tokenPrefix = "boat_"

// NewToken returns a key id, its secret and the full bearer token.
func NewToken() (id, secret, token string) {
	id, secret = randHex(8), randHex(32)
	return id, secret, tokenPrefix + id + "_" + secret
}

func ParseToken(tok string) (id, secret string, ok bool) {
	rest, found := strings.CutPrefix(tok, tokenPrefix)
	if !found {
		return "", "", false
	}
	id, secret, found = strings.Cut(rest, "_")
	if !found || len(id) != 16 || len(secret) != 64 || !isHex(id) || !isHex(secret) {
		return "", "", false
	}
	return id, secret, true
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

// The desktop key file (-desktop-key-file, shared with boat-gateway) is a
// master key: each kind of token is signed with its own key derived from it,
// HMAC-SHA256(master, "boat-hmac-v2/<purpose>"), so a token of one kind can
// never verify as another whatever its payload. boat-gateway derives the same
// keys (gateway/auth.go). Tokens minted before the split were signed with the
// master key itself; the gateway accepts those until they expire
// (-legacy-hmac-until), then never again.
const (
	purposeDesktopLink  = "desktop-link"
	purposePortRoute    = "port-route"
	purposeGuestDesktop = "guest-desktop"
)

// PurposeKey is the key for one kind of token.
func PurposeKey(master []byte, purpose string) []byte {
	return derive(master, "boat-hmac-v2/"+purpose)
}

// SignDesktop: sig = hex(HMAC-SHA256(key, vmID + "." + exp)). The desktop
// gateway verifies with the same shared key file and rejects exp < now. Once
// the VM's links were revoked (gen > 0, the port routes' RouteGens) the payload
// is vmID + ".g<gen>." + exp, which kills every earlier link and cookie.
func SignDesktop(key []byte, vmID string, gen int, exp int64) string {
	m := hmac.New(sha256.New, key)
	if gen == 0 {
		fmt.Fprintf(m, "%s.%d", vmID, exp)
	} else {
		fmt.Fprintf(m, "%s.g%d.%d", vmID, gen, exp)
	}
	return hex.EncodeToString(m.Sum(nil))
}

// SignWebhook: X-Boat-Signature: t=<unix>,v1=hex(HMAC-SHA256(secret, "<t>." + body)).
// During a rotation overlap, one more v1= per extra secret is appended; a
// receiver accepts the delivery if any v1 matches.
func SignWebhook(secret []byte, ts int64, body []byte, older ...[]byte) string {
	out := fmt.Sprintf("t=%d", ts)
	for _, sec := range append([][]byte{secret}, older...) {
		m := hmac.New(sha256.New, sec)
		fmt.Fprintf(m, "%d.", ts)
		m.Write(body)
		out += ",v1=" + hex.EncodeToString(m.Sum(nil))
	}
	return out
}

// SignGuestDesktop is the token the desktop gateway sends to the guest proxy:
// hex(HMAC-SHA256(key, "guest-desktop.<vmID>")).
func SignGuestDesktop(key []byte, vmID string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("guest-desktop." + vmID))
	return hex.EncodeToString(m.Sum(nil))
}

func signHex(key []byte, payload string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(payload))
	return hex.EncodeToString(m.Sum(nil))
}

// isCredential reports whether path is a systemd credential. systemd manages
// that directory per service; when it grants the service user access through
// an ACL the file's group bits show the ACL mask (0440), so the mode check
// for plain key files does not apply.
func isCredential(path string) bool {
	d := os.Getenv("CREDENTIALS_DIRECTORY")
	return d != "" && filepath.Dir(path) == filepath.Clean(d)
}
