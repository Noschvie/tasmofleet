package notify

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
	"math/big"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hellwand/tasmofleet/internal/config"
)

func b64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("test vector %q: %v", s, err)
	}
	return b
}

// RFC 8291, Appendix A (whitespace removed).
const (
	rfcPlaintext  = "When I grow up, I want to be a watermelon"
	rfcASPrivate  = "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"
	rfcUAPublic   = "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"
	rfcUAPrivate  = "q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94"
	rfcSalt       = "DGv6ra1nlYgDCS1FRnbzlw"
	rfcAuth       = "BTBZMqHH6r4Tts7J_aSIgg"
	rfcHeader     = "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8"
	rfcCiphertext = "8pfeW0KbunFT06SuDKoJH9Ql87S1QUrdirN6GcG7sFz1y1sqLgVi1VhjVkHsUoEsbI_0LpXMuGvnzQ"
)

// Why (#610, acceptance): a push service passes the body through, and the browser drops a message it
// cannot decrypt without telling anybody. The only proof that the encryption is right is the exact
// byte sequence of the RFC's own example.
func TestPushEncryptionMatchesRFC8291Vector(t *testing.T) {
	as, err := ecdh.P256().NewPrivateKey(b64(t, rfcASPrivate))
	if err != nil {
		t.Fatal(err)
	}
	got, err := encryptPush(b64(t, rfcUAPublic), b64(t, rfcAuth), []byte(rfcPlaintext), as, b64(t, rfcSalt))
	if err != nil {
		t.Fatal(err)
	}
	want := append(b64(t, rfcHeader), b64(t, rfcCiphertext)...)
	if !bytes.Equal(got, want) {
		t.Fatalf("body differs from RFC 8291 Appendix A:\n got %s\nwant %s", base64.RawURLEncoding.EncodeToString(got), base64.RawURLEncoding.EncodeToString(want))
	}
	// the receiver helper the other tests use reads the RFC's body back (so it is a fair judge)
	if pt := decryptPush(t, b64(t, rfcUAPrivate), b64(t, rfcAuth), want); string(pt) != rfcPlaintext {
		t.Fatalf("decrypt of the RFC body: %q", pt)
	}
}

// decryptPush is the browser's side of RFC 8291, written from the RFC for the tests.
func decryptPush(t *testing.T, uaPriv, auth, body []byte) []byte {
	t.Helper()
	if len(body) < 86 {
		t.Fatalf("body of %d bytes has no header", len(body))
	}
	salt, rs, idlen := body[:16], binary.BigEndian.Uint32(body[16:20]), int(body[20])
	if idlen != 65 || rs != 4096 {
		t.Fatalf("header: idlen %d rs %d", idlen, rs)
	}
	asPub, rec := body[21:21+idlen], body[21+idlen:]
	if len(rec) > int(rs) {
		t.Fatalf("record of %d bytes exceeds rs", len(rec))
	}
	priv, err := ecdh.P256().NewPrivateKey(uaPriv)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ecdh.P256().NewPublicKey(asPub)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := priv.ECDH(pub)
	if err != nil {
		t.Fatal(err)
	}
	info := append(append([]byte("WebPush: info\x00"), priv.PublicKey().Bytes()...), asPub...)
	ikm, _ := hkdf.Key(sha256.New, secret, auth, string(info), 32)
	cek, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	blk, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(blk)
	pt, err := gcm.Open(nil, nonce, rec, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	pt = bytes.TrimRight(pt, "\x00")
	if len(pt) == 0 || pt[len(pt)-1] != 0x02 {
		t.Fatal("no last-record delimiter")
	}
	return pt[:len(pt)-1]
}

// Why (RFC 8292): the push service accepts a push only with a token signed by the key the browser
// subscribed with (k). The audience must be the push service's origin alone, so a token cannot be
// replayed elsewhere, and the token must expire within the bound this code promises (12 hours; the RFC
// allows at most 24).
func TestVAPIDTokenVerifiesForTheEndpointOrigin(t *testing.T) {
	key, err := NewVAPIDKey()
	if err != nil {
		t.Fatal(err)
	}
	priv, err := parseVAPIDKey(key)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	h, err := vapidAuth(priv, "https://push.example.net:8443/wpush/v2/secret-part?x=1", now)
	if err != nil {
		t.Fatal(err)
	}
	tok, k, ok := strings.Cut(strings.TrimPrefix(h, "vapid t="), ", k=")
	if !strings.HasPrefix(h, "vapid t=") || !ok {
		t.Fatalf("header %q", h)
	}
	if k != publicKeyB64(priv) {
		t.Fatalf("k %q is not the public key %q", k, publicKeyB64(priv))
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), b64(t, k))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token %q", tok)
	}
	sig := b64(t, parts[2])
	if len(sig) != 64 {
		t.Fatalf("ES256 signature of %d bytes", len(sig))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("signature does not verify with k")
	}
	var hdr struct{ Typ, Alg string }
	if err := json.Unmarshal(b64(t, parts[0]), &hdr); err != nil || hdr.Alg != "ES256" || hdr.Typ != "JWT" {
		t.Fatalf("header %+v %v", hdr, err)
	}
	var claims struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(b64(t, parts[1]), &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Aud != "https://push.example.net:8443" {
		t.Errorf("aud %q, want the endpoint origin only", claims.Aud)
	}
	if exp := time.Unix(claims.Exp, 0); exp.After(now.Add(12*time.Hour)) || !exp.After(now) {
		t.Errorf("exp %v, want after now and at most 12 h ahead", exp)
	}
	// Apple refuses a token without a contact; it is the fixed project address, never PUBLIC_URL or a mail address
	if claims.Sub != "https://github.com/hellwand/tasmofleet" {
		t.Errorf("sub %q", claims.Sub)
	}
	if strings.Contains(h, key) {
		t.Error("the header carries the private key")
	}
}

// Why: the key is stored as text; reading it back must give the same key, or every subscription breaks
// after a restart.
func TestVAPIDKeyRoundTrip(t *testing.T) {
	key, err := NewVAPIDKey()
	if err != nil {
		t.Fatal(err)
	}
	a, err := parseVAPIDKey(key)
	if err != nil {
		t.Fatal(err)
	}
	b, err := parseVAPIDKey(key)
	if err != nil || publicKeyB64(a) != publicKeyB64(b) || len(b64(t, publicKeyB64(a))) != 65 {
		t.Fatalf("round trip: %v", err)
	}
	if _, err := parseVAPIDKey("not-a-key"); err == nil {
		t.Fatal("garbage parsed as a key")
	}
}

func testSubscriber(t *testing.T) (priv, pub, auth []byte) {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth = make([]byte, 16)
	_, _ = rand.Read(auth) // never fails (crypto/rand)
	return k.Bytes(), k.PublicKey().Bytes(), auth
}

// Why: push services refuse a body over 4096 bytes, and a backup failure lists up to ten devices with
// reasons. The text is cut to fit, never the JSON, and never inside a character (a broken UTF-8 sequence
// makes the browser's JSON.parse fail and the notification is lost). The payload carries only what the
// tab notice shows: no URL with the public address, no context, nothing else.
func TestPushPayloadCarriesOnlyNoticeFieldsAndFits(t *testing.T) {
	c := config.Config{BasePath: "/tf/", PublicURL: "https://fleet.example"}
	long := strings.Repeat("ä€ device kitchen-lamp failed: timed out after 10 s\n", 200)
	for name, ev := range map[string]Event{
		"short": {ID: "1a-1", Name: EventBackupFailed, Result: ResultFailed, Title: "Scheduled backup: 1 of 3 devices failed", Text: "Failed:\n- lamp", Path: "/devices/backups?filter=failed", URL: "https://fleet.example/tf/x", Context: "scheduled backup"},
		"long":  {ID: "1a-2", Name: EventBackupFailed, Result: ResultFailed, Title: "T", Text: long, Path: "/devices/backups"},
	} {
		b := pushPayload(c, ev)
		if b == nil {
			t.Fatalf("%s: no payload", name)
		}
		if len(b) > maxPushPlaintext {
			t.Fatalf("%s: payload %d bytes > %d", name, len(b), maxPushPlaintext)
		}
		if !utf8.Valid(b) {
			t.Fatalf("%s: payload is not valid UTF-8", name)
		}
		var m map[string]string
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(m) != 6 || m["id"] != ev.ID || m["name"] != ev.Name || m["result"] != ev.Result || m["title"] != ev.Title {
			t.Fatalf("%s: fields %v", name, m)
		}
		if m["path"] != "/tf"+ev.Path {
			t.Errorf("%s: path %q, want it below the base path", name, m["path"])
		}
		if name == "short" && m["text"] != ev.Text {
			t.Errorf("short text was changed: %q", m["text"])
		}
		if name == "long" && (!strings.HasPrefix(long, strings.TrimSuffix(m["text"], "…")) || !strings.HasSuffix(m["text"], "…") || len(b) < maxPushPlaintext-8) {
			t.Errorf("long text not cut to fit with a mark: %d bytes", len(b))
		}
		// the whole encrypted body stays within the 4096 bytes the push services take
		priv, pub, auth := testSubscriber(t)
		body, err := encryptPayload(pub, auth, b)
		if err != nil {
			t.Fatal(err)
		}
		if len(body) > 4096 {
			t.Fatalf("%s: body %d bytes", name, len(body))
		}
		if got := decryptPush(t, priv, auth, body); !bytes.Equal(got, b) {
			t.Fatalf("%s: decrypted payload differs", name)
		}
	}
}

// Why: once the text and the title are cut away there is nothing left to cut. When the fields that are
// never cut (id, name, result and the path below a very long base path) alone do not fit, pushPayload must
// say so (nil) instead of cutting nothing forever in a dispatcher goroutine that shutdown then waits for.
func TestPushPayloadThatCannotFitIsNil(t *testing.T) {
	c := config.Config{BasePath: strings.Repeat("/x", 2100)}
	ev := Event{ID: "1a-3", Name: EventBackupFailed, Result: ResultFailed, Title: "T", Text: "text", Path: "/devices/backups"}
	done := make(chan []byte, 1)
	go func() { done <- pushPayload(c, ev) }()
	select {
	case b := <-done:
		if b != nil {
			t.Fatalf("payload of %d bytes for an event that cannot fit", len(b))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pushPayload did not return for an event that cannot fit")
	}
}

// Why: the limit is exact. 3993 bytes of plaintext make a body of exactly 4096; one more byte would be
// refused by the push service.
func TestPushPlaintextBoundary(t *testing.T) {
	if maxPushPlaintext != 3993 {
		t.Fatalf("maxPushPlaintext = %d, want 4096 - 86 (header) - 1 (delimiter) - 16 (tag)", maxPushPlaintext)
	}
	_, pub, auth := testSubscriber(t)
	body, err := encryptPayload(pub, auth, bytes.Repeat([]byte("x"), maxPushPlaintext))
	if err != nil || len(body) != 4096 {
		t.Fatalf("body %d bytes, %v; want exactly 4096", len(body), err)
	}
	if _, err := encryptPayload(pub, auth, bytes.Repeat([]byte("x"), maxPushPlaintext+1)); err == nil {
		t.Fatal("a plaintext over the limit was encrypted")
	}
	// a text whose cut would fall inside a three-byte character keeps whole characters only
	for pad := range 3 {
		c := config.Config{}
		ev := Event{ID: "x", Name: EventSnapshotFailed, Result: ResultFailed, Title: "t", Text: strings.Repeat("a", pad) + strings.Repeat("€", 2000)}
		b := pushPayload(c, ev)
		if len(b) > maxPushPlaintext || !utf8.Valid(b) {
			t.Fatalf("pad %d: %d bytes, valid %v", pad, len(b), utf8.Valid(b))
		}
	}
}

// Why: the subscription comes from a browser through an admin's request. Only an https push service
// address without credentials can be a push service, a key that is not a P-256 point or an auth secret
// of the wrong size would make every push fail, and an address that is plainly this host or the LAN (in any
// spelling Go dials: a trailing dot, upper case, a zone) is a mistake no push service makes. Host names are
// not resolved: this is a check against mistakes, not an SSRF boundary (W5).
func TestSubscriptionCheckRefusesBadEndpointsAndKeys(t *testing.T) {
	_, pub, auth := testSubscriber(t)
	p, a := base64.RawURLEncoding.EncodeToString(pub), base64.RawURLEncoding.EncodeToString(auth)
	good := "https://fcm.googleapis.com/fcm/send/abc:def"
	if err := CheckSubscription(good, p, a); err != nil {
		t.Fatalf("valid subscription refused: %v", err)
	}
	if err := CheckSubscription(good, base64.URLEncoding.EncodeToString(pub), base64.URLEncoding.EncodeToString(auth)); err != nil {
		t.Fatalf("padded base64url refused: %v", err)
	}
	notOnCurve := append([]byte{4}, bytes.Repeat([]byte{1}, 64)...)
	for name, tc := range map[string][3]string{
		"http":       {"http://push.example/x", p, a},
		"userinfo":   {"https://user:pw@push.example/x", p, a},
		"no host":    {"https:///x", p, a},
		"too long":   {"https://push.example/" + strings.Repeat("x", 2048), p, a},
		"loopback":   {"https://127.0.0.1/x", p, a},
		"localhost":  {"https://localhost:8443/x", p, a},
		"private ip": {"https://192.168.1.10/x", p, a},
		"ipv6 ula":   {"https://[fd00::1]/x", p, a},
		"link local": {"https://169.254.1.1/x", p, a},
		// the same hosts spelt so that a plain string or net.ParseIP test misses them; Go dials them all
		"localhost dot":       {"https://localhost./x", p, a},
		"localhost upper dot": {"https://LOCALHOST./x", p, a},
		"loopback zone":       {"https://[::1%25lo]/x", p, a},
		"link local zone":     {"https://[fe80::1%25eth0]/x", p, a},
		"cgnat":               {"https://100.64.0.1/x", p, a},
		"not a url":           {"https://push.example/%zz", p, a},
		"short p256dh":        {good, base64.RawURLEncoding.EncodeToString(pub[:33]), a},
		"off curve":           {good, base64.RawURLEncoding.EncodeToString(notOnCurve), a},
		"bad base64":          {good, "!!!", a},
		"auth size":           {good, p, base64.RawURLEncoding.EncodeToString(auth[:8])},
	} {
		if err := CheckSubscription(tc[0], tc[1], tc[2]); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
