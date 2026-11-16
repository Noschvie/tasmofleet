package notify

// Web Push, the pure part (#610): the VAPID key and token (RFC 8292), the payload encryption (RFC 8291,
// aes128gcm, one record) and the check of a subscription a browser sends. Standard library only: crypto/ecdh,
// crypto/ecdsa, crypto/hkdf and AES-GCM cover it, and TestPushEncryptionMatchesRFC8291Vector holds the
// encryption to the RFC's own example (spec 3.3).

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hellwand/tasmofleet/internal/config"
)

const (
	// maxPushBody is what the push services take (FCM, Mozilla autopush, Apple: 4096 bytes).
	maxPushBody = 4096
	// pushRecordSize is the rs field of the header; the one record is never larger.
	pushRecordSize = 4096
	// p256PointLen is an uncompressed P-256 public key.
	p256PointLen = 65
	// pushHeaderLen is salt (16) + rs (4) + idlen (1) + the server's public key.
	pushHeaderLen = 16 + 4 + 1 + p256PointLen
	// maxPushPlaintext leaves room for the header, the delimiter octet and the GCM tag: 3993.
	maxPushPlaintext = maxPushBody - pushHeaderLen - 1 - 16
	// vapidTTL is how long a VAPID token is valid (RFC 8292 allows at most 24 hours).
	vapidTTL = 12 * time.Hour
	// vapidSubject is the contact the token names. It is the fixed project address on purpose: PUBLIC_URL
	// or a mail address would tell the browser vendors an internal host name or an address.
	vapidSubject = "https://github.com/hellwand/tasmofleet"
	// maxEndpointLen bounds the address a browser sends.
	maxEndpointLen = 2048
)

// NewVAPIDKey makes a P-256 key pair and returns its private key as base64url text (the raw scalar), the
// form the store seals.
func NewVAPIDKey() (string, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	b, err := k.Bytes()
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func parseVAPIDKey(s string) (*ecdsa.PrivateKey, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, errors.New("the VAPID key is not base64url")
	}
	return ecdsa.ParseRawPrivateKey(elliptic.P256(), b)
}

// publicKeyB64 is the public key as the browser's applicationServerKey wants it: the uncompressed point,
// base64url without padding.
func publicKeyB64(k *ecdsa.PrivateKey) string {
	b, err := k.PublicKey.Bytes()
	if err != nil {
		return "" // unreachable: a parsed P-256 key always encodes
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// vapidAuth is the Authorization header of a push to endpoint (RFC 8292): an ES256 JWT for the endpoint's
// origin, valid for vapidTTL, and the public key.
func vapidAuth(k *ecdsa.PrivateKey, endpoint string, now time.Time) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", errors.New("the push address is not valid")
	}
	claims, err := json.Marshal(struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}{u.Scheme + "://" + u.Host, now.Add(vapidTTL).Unix(), vapidSubject})
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`)) + "." + enc.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, k, digest[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return "vapid t=" + signing + "." + enc.EncodeToString(sig) + ", k=" + publicKeyB64(k), nil
}

// encryptPayload encrypts plaintext for a browser with a fresh key pair and salt.
func encryptPayload(uaPublic, authSecret, plaintext []byte) ([]byte, error) {
	as, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return encryptPush(uaPublic, authSecret, plaintext, as, salt)
}

// encryptPush is RFC 8291 with the aes128gcm content coding (RFC 8188) in one record, no padding:
// header (salt, rs, the server's public key) followed by the record. as and salt are the server's
// one-time key and the salt (fixed only in the RFC test).
func encryptPush(uaPublic, authSecret, plaintext []byte, as *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	if len(plaintext) > maxPushPlaintext {
		return nil, fmt.Errorf("push payload of %d bytes is over %d", len(plaintext), maxPushPlaintext)
	}
	if len(authSecret) != 16 || len(salt) != 16 {
		return nil, errors.New("push encryption: auth secret and salt are 16 bytes")
	}
	ua, err := ecdh.P256().NewPublicKey(uaPublic)
	if err != nil {
		return nil, errors.New("push encryption: the browser key is not a P-256 point")
	}
	secret, err := as.ECDH(ua)
	if err != nil {
		return nil, err
	}
	asPublic := as.PublicKey().Bytes()
	keyInfo := append(append([]byte("WebPush: info\x00"), uaPublic...), asPublic...)
	ikm, err := hkdf.Key(sha256.New, secret, authSecret, string(keyInfo), 32)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	blk, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.Write(salt)
	_ = binary.Write(&out, binary.BigEndian, uint32(pushRecordSize)) // a bytes.Buffer write cannot fail
	out.WriteByte(p256PointLen)                                      // idlen: an uncompressed P-256 point, always 65 bytes
	out.Write(asPublic)
	record := append(append(make([]byte, 0, len(plaintext)+1), plaintext...), 0x02) // 0x02: the last record
	return gcm.Seal(out.Bytes(), nonce, record, nil), nil
}

// pushPayload is the JSON the service worker shows: the fields of the tab notice (#540) and nothing else,
// the path below the base path. A text too long for one push is cut at a character boundary and marked.
// It is nil when the event does not fit even with the text and the title cut away.
func pushPayload(c config.Config, ev Event) []byte {
	path := ""
	if ev.Path != "" {
		path = strings.TrimRight(c.BasePath, "/") + ev.Path
	}
	m := struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Result string `json:"result"`
		Title  string `json:"title"`
		Text   string `json:"text"`
		Path   string `json:"path"`
	}{ev.ID, ev.Name, ev.Result, ev.Title, ev.Text, path}
	for {
		b, err := json.Marshal(m)
		if err != nil {
			return nil // unreachable: plain strings
		}
		over := len(b) - maxPushPlaintext
		if over <= 0 {
			return b
		}
		// Cut the text first, then the title. A byte of text costs one to six bytes of JSON (escapes), so
		// cutting over/6 bytes never cuts more than needed; the loop runs until it fits.
		if m.Text != "" {
			m.Text = cutRunes(strings.TrimSuffix(m.Text, "…"), (over+5)/6+len("…")) + "…"
			if m.Text == "…" {
				m.Text = ""
			}
			continue
		}
		if m.Title == "" {
			return nil // the fields that are never cut (id, name, result, path) alone do not fit
		}
		m.Title = cutRunes(m.Title, over)
	}
}

// cutRunes drops at least n bytes from the end of s, never inside a character.
func cutRunes(s string, n int) string {
	k := max(len(s)-n, 0)
	for k > 0 && !utf8.RuneStart(s[k]) {
		k--
	}
	return s[:k]
}

// sharedAddressSpace is 100.64.0.0/10 (RFC 6598, carrier-grade NAT and Tailscale), not counted as private.
var sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")

// ErrBadSubscription is what CheckSubscription returns; the message says what is wrong.
var ErrBadSubscription = errors.New("not a valid push subscription")

// CheckSubscription validates what a browser sends to subscribe: an https address of a push service
// (no credentials in it, not this host or a private network, at most 2 KB), the browser's P-256 public key
// and its 16-byte auth secret, both base64url.
func CheckSubscription(endpoint, p256dh, auth string) error {
	bad := func(why string) error { return fmt.Errorf("%w: %s", ErrBadSubscription, why) }
	if len(endpoint) > maxEndpointLen {
		return bad("the address is too long")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" {
		return bad("the address must be an https URL")
	}
	if u.User != nil {
		return bad("the address must not carry credentials")
	}
	// A check against mistakes, not an SSRF boundary: host names are not resolved (docs/security/web-push.md W5).
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return bad("the address must be a push service on the internet")
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		// netip parses zones (net.ParseIP returns nil on them); no push service has a zone.
		if a := ip.Unmap(); ip.Zone() != "" || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsUnspecified() || a.IsMulticast() || sharedAddressSpace.Contains(a) {
			return bad("the address must be a push service on the internet")
		}
	}
	pub, err := decodeB64URL(p256dh)
	if err != nil {
		return bad("the browser key is not base64url")
	}
	if _, err := ecdh.P256().NewPublicKey(pub); err != nil {
		return bad("the browser key is not a P-256 public key")
	}
	a, err := decodeB64URL(auth)
	if err != nil || len(a) != 16 {
		return bad("the auth secret is not 16 bytes of base64url")
	}
	return nil
}

// decodeB64URL reads base64url with or without padding (browsers send it without).
func decodeB64URL(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}
