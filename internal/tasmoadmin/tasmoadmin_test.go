package tasmoadmin

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtures = "../../testdata/fixtures/tasmoadmin"

func openFixture(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open(filepath.Join(fixtures, name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// encrypt mirrors TasmoAdmin's layout: enc:v1: + base64(iv || tag || ciphertext).
// Go's GCM appends the tag to the ciphertext, so the pieces are re-ordered.
func encrypt(t *testing.T, key []byte, plain string) string {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	iv := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(iv); err != nil {
		t.Fatal(err)
	}
	sealed := gcm.Seal(nil, iv, []byte(plain), nil)
	ct, tag := sealed[:len(sealed)-gcm.Overhead()], sealed[len(sealed)-gcm.Overhead():]
	raw := append(append(append([]byte{}, iv...), tag...), ct...)
	return "enc:v1:" + base64.StdEncoding.EncodeToString(raw)
}

func newKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

// Why: TasmoAdmin stores a web user for every device, also when the device has
// no password. A user without a password is no credential (spec 11.1); keeping
// it would make every imported device look protected.
func TestUserWithoutPasswordIsDroppedOnImport(t *testing.T) {
	csv := "1,\"A\",192.168.1.50,admin,,bulb_1,1,1,0,0,1,80,\"A\",0,a,0\n" +
		"2,\"B\",192.168.1.51,admin,plainpw,bulb_1,1,1,0,0,1,80,\"B\",0,b,0\n"
	devs, _, err := ParseDevices(strings.NewReader(csv), nil)
	if err != nil || len(devs) != 2 {
		t.Fatalf("%v %d", err, len(devs))
	}
	if devs[0].HTTPUser != "" || devs[0].HasCredentials() {
		t.Errorf("user without password kept: %+v", devs[0])
	}
	if devs[1].HTTPUser != "admin" || devs[1].HTTPPassword != "plainpw" {
		t.Errorf("credentials lost: %+v", devs[1])
	}
}

// The documented results of the sample file: nothing may be dropped silently,
// and every row that needs the operator's attention produces an error.
func TestParseDevicesFixtureWithoutKey(t *testing.T) {
	devs, errs, err := ParseDevices(openFixture(t, "devices.csv"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 4 {
		t.Fatalf("want 4 devices, got %d", len(devs))
	}

	d := devs[0]
	if d.Name != "Kitchen Light" || d.Address != "192.168.1.50" || d.Port != 80 ||
		d.HTTPUser != "" || d.HTTPPassword != "" || d.Topic != "kitchen_light" || d.OTAExcluded {
		t.Errorf("row 1 wrong: %+v", d)
	}

	// Row 2: encrypted, no key -> password stays empty and the row is reported,
	// otherwise the operator would find out only when the device answers 401.
	d = devs[1]
	if d.HTTPPassword != "" || d.Topic != "desk_strip" {
		t.Errorf("row 2 wrong: %+v", d)
	}
	var re *RowError
	found := false
	for _, e := range errs {
		if errors.As(e, &re) && re.Line == 2 && errors.Is(e, ErrEncryptedNoKey) {
			found = true
		}
	}
	if !found {
		t.Errorf("row 2 not reported as encrypted without key: %v", errs)
	}

	// Row 3: legacy plaintext, short row, port in column 11.
	d = devs[2]
	if d.HTTPPassword != "password" || d.HTTPUser != "user" || d.Port != 5000 || d.OTAExcluded || d.Topic != "" {
		t.Errorf("row 3 wrong: %+v", d)
	}
	if d.Name != "socket-1" {
		t.Errorf("row 3 name: %q", d.Name)
	}

	// Row 4: backslash-escaped quotes (fputcsv escape char), 7 columns.
	if devs[3].Name != `Old "quoted" plug` {
		t.Errorf("row 4 name: %q", devs[3].Name)
	}
	if devs[3].Port != 80 {
		t.Errorf("row 4 port default: %d", devs[3].Port)
	}
}

func TestParseDevicesDecryptsWithGeneratedKey(t *testing.T) {
	key := newKey(t)
	csv := `7,"Plug",10.0.0.7,admin,` + encrypt(t, key, `p&ss"w,rd`) + `,bulb_1,1,1,0,0,1,80,"Plug",0,plug,0` + "\n"

	for name, k := range map[string][]byte{
		"raw key":       key,
		"key file text": []byte(base64.StdEncoding.EncodeToString(key) + "\n"),
		"base64 no \\n": []byte(base64.StdEncoding.EncodeToString(key)),
	} {
		t.Run(name, func(t *testing.T) {
			devs, errs, err := ParseDevices(strings.NewReader(csv), k)
			if err != nil || len(errs) != 0 {
				t.Fatalf("err=%v errs=%v", err, errs)
			}
			if devs[0].HTTPPassword != `p&ss"w,rd` {
				t.Errorf("password not decrypted (special characters must survive): %q", devs[0].HTTPPassword)
			}
		})
	}
}

// A wrong key must never yield garbage that is later sent to a device.
func TestParseDevicesWrongKeyReportsAndClearsPassword(t *testing.T) {
	csv := `7,"Plug",10.0.0.7,admin,` + encrypt(t, newKey(t), "secret") + "\n"
	devs, errs, err := ParseDevices(strings.NewReader(csv), newKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if devs[0].HTTPPassword != "" {
		t.Error("password must be empty after failed decryption")
	}
	if len(errs) != 1 || !errors.Is(errs[0], ErrDecrypt) {
		t.Errorf("want one ErrDecrypt, got %v", errs)
	}
	for _, e := range errs {
		if strings.Contains(e.Error(), "secret") {
			t.Error("error leaks password")
		}
	}
}

func TestParseDevicesInvalidKeyIsFatal(t *testing.T) {
	_, _, err := ParseDevices(strings.NewReader("1,a,1.2.3.4\n"), []byte("short"))
	if err == nil {
		t.Fatal("a key that is neither 32 raw bytes nor base64 of 32 bytes must fail the import")
	}
}

func TestParseDevicesRowProblems(t *testing.T) {
	csv := strings.Join([]string{
		`1,"No IP",,admin,,bulb_1,1`, // line 1: no address
		``,                           // blank line is not a row
		`3,"Off OTA",10.0.0.3,,,bulb_1,3,1,0,0,0,80`, // isUpdatable=0
		`4,"Bad port",10.0.0.4,,,bulb_1,4,1,0,0,1,notaport`,
		`5,"Quote ""doubled""",10.0.0.5`, // doubled quote also valid CSV
		`6`,                              // too short to carry an address
		`7,"Friendly",10.0.0.7,,,bulb_1,7,1,0,0,1,80,"Pretty Name|Second",0,t7`,
		`8,"Desk Left|Desk Right",10.0.0.8,,,bulb_1,8`,
	}, "\n") + "\n"
	devs, errs, err := ParseDevices(strings.NewReader(csv), nil)
	if err != nil {
		t.Fatal(err)
	}

	byAddr := map[string]int{}
	for i, d := range devs {
		byAddr[d.Address] = i
	}
	if _, ok := byAddr[""]; ok || len(devs) != 5 {
		t.Fatalf("rows without address must be reported, not imported: %d devices", len(devs))
	}
	if !devs[byAddr["10.0.0.3"]].OTAExcluded {
		t.Error("isUpdatable=0 must map to OTAExcluded")
	}
	if devs[byAddr["10.0.0.4"]].Port != 80 {
		t.Error("unparsable port falls back to 80")
	}
	if devs[byAddr["10.0.0.5"]].Name != `Quote "doubled"` {
		t.Errorf("doubled quote: %q", devs[byAddr["10.0.0.5"]].Name)
	}
	if devs[byAddr["10.0.0.7"]].Name != "Pretty Name-Second" {
		t.Errorf("friendly names (col 12) win over relay names: %q", devs[byAddr["10.0.0.7"]].Name)
	}
	if devs[byAddr["10.0.0.8"]].Name != "Desk Left-Desk Right" {
		t.Errorf("relay names are joined like TasmoAdmin does: %q", devs[byAddr["10.0.0.8"]].Name)
	}

	lines := map[int]error{}
	for _, e := range errs {
		var re *RowError
		if errors.As(e, &re) {
			lines[re.Line] = e
		}
	}
	for _, want := range []struct {
		line int
		is   error
	}{{1, ErrNoAddress}, {6, ErrNoAddress}} {
		if !errors.Is(lines[want.line], want.is) {
			t.Errorf("line %d: want %v, got %v", want.line, want.is, lines[want.line])
		}
	}
	if lines[4] == nil {
		t.Error("bad port must be reported")
	}
}

func TestParseDevicesUnterminatedQuote(t *testing.T) {
	devs, errs, err := ParseDevices(strings.NewReader("1,\"broken,10.0.0.1\n"), nil)
	if err != nil {
		t.Fatalf("a damaged row is a row error, not a fatal one: %v", err)
	}
	if len(devs) != 0 || len(errs) == 0 {
		t.Errorf("devs=%v errs=%v", devs, errs)
	}
}

func TestParseConfigFixture(t *testing.T) {
	c, err := ParseConfig(openFixture(t, "MyConfig.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Only the keys the importer needs; the login hash is never imported (spec 11.2).
	if c.OtaServerIP != "192.168.1.10" || c.OtaServerPort != "80" {
		t.Errorf("ota server: %+v", c)
	}
	if c.DefaultUsername != "" || c.DefaultPassword != "" {
		t.Errorf("TasmoAdmin has no default device credentials in MyConfig; got %+v", c)
	}
}

func TestParseConfigTolerance(t *testing.T) {
	c, err := ParseConfig(strings.NewReader(`{"ota_server_port": 8080, "ota_server_ip": "10.1.1.1", "unknown": {"x": 1}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.OtaServerPort != "8080" || c.OtaServerIP != "10.1.1.1" {
		t.Errorf("numbers must be accepted where TasmoAdmin normally writes strings: %+v", c)
	}
	if _, err := ParseConfig(strings.NewReader(`[1]`)); err == nil {
		t.Error("non-object must fail")
	}
}

// The file comes from another system; whatever it contains, parsing must
// neither panic nor return a device without an address.
func FuzzParseDevices(f *testing.F) {
	for _, seed := range []string{"1,a,1.2.3.4\n", "\"x\\\"y\",,\n", "1,\"unterminated", ",,,,\r\n\r\n", "1,a,1.2.3.4,u,enc:v1:AAAA\n"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		devs, _, err := ParseDevices(strings.NewReader(in), nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range devs {
			if d.Address == "" {
				t.Fatalf("device without address: %+v", d)
			}
		}
	})
}

// Why (review F1): the parser is fed uploads; record count, field count and
// field size are bounded so a small file cannot amplify into memory.
func TestParseDevicesBoundsRecordsAndFields(t *testing.T) {
	_, errs, err := ParseDevices(strings.NewReader(strings.Repeat("x\n", 5000)), nil)
	if err != nil || len(errs) > maxRecords+1 || !errors.Is(errors.Join(errs...), ErrTooManyRecords) {
		t.Fatalf("records: err=%v errs=%d", err, len(errs))
	}
	long := "1,\"" + strings.Repeat("a", maxFieldBytes+10) + "\",10.0.0.1\n" + strings.Repeat("1,", maxFields+2) + "\n"
	_, errs, err = ParseDevices(strings.NewReader(long), nil)
	if err != nil || len(errs) != 2 || !errors.Is(errs[0], ErrRecordTooBig) || !errors.Is(errs[1], ErrRecordTooBig) {
		t.Fatalf("fields: err=%v errs=%v", err, errs)
	}
}
