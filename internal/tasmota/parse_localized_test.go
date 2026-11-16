package tasmota

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #468: Tasmota builds the main page footer as "Tasmota <version> <image> " D_BY " Theo Arends"
// (xdrv_01_9_webserver.ino, HTTP_END), and every language build translates D_BY. A device on a
// localized build (tasmota-FR.bin ...) was refused as "no Tasmota footer", so it could neither be
// identified nor updated. The parser must not depend on any localized word.

// dByWords lists D_BY of every language header in tasmota/language (Tasmota development branch,
// a281cec, 2026-11-05) with the file it comes from. Add a line when upstream adds a language.
var dByWords = []struct{ file, word string }{
	{"af_AF.h", "deur"}, {"bg_BG.h", "от"}, {"ca_AD.h", "per"}, {"cs_CZ.h", "by"},
	{"de_DE.h", "von"}, {"el_GR.h", "από"}, {"en_GB.h", "by"}, {"es_ES.h", "por"},
	{"fr_FR.h", "par"}, {"fy_NL.h", "troch"}, {"he_HE.h", "על ידי"}, {"hu_HU.h", "by"},
	{"it_IT.h", "di"}, {"ko_KO.h", "by"}, {"lt_LT.h", "autorius"}, {"nl_NL.h", "door"},
	{"pl_PL.h", "przez"}, {"pt_BR.h", "por"}, {"pt_PT.h", "por"}, {"ro_RO.h", "de"},
	{"ru_RU.h", "by"}, {"sk_SK.h", "by"}, {"sv_SE.h", "av"}, {"tr_TR.h", "by"},
	{"uk_UA.h", " "}, {"vi_VN.h", "bởi"}, {"zh_CN.h", "作者:"}, {"zh_TW.h", "由"},
}

func footerPage(by string) string {
	return `<div style='text-align:right;font-size:11px;'><hr><a href='https://github.com/arendst/Tasmota' target='_blank' style='color:#aaa;'>` +
		"Tasmota 15.1.0 (release-tasmota) " + by + " Theo Arends</a></div>"
}

func TestParseRootPage_FooterInEveryLanguage(t *testing.T) {
	for _, l := range dByWords {
		t.Run(l.file, func(t *testing.T) {
			got, err := ParseRootPage([]byte(footerPage(l.word)))
			if err != nil {
				t.Fatalf("footer with D_BY %q: %v", l.word, err)
			}
			if got.Version != "15.1.0" || got.Image != "release-tasmota" {
				t.Errorf("version %q image %q", got.Version, got.Image)
			}
		})
	}
}

// Real main pages of localized builds, built from the English capture (README of the fixtures).
func TestParseRootPage_LocalizedFixtures(t *testing.T) {
	for _, name := range []string{"de_DE", "fr_FR"} {
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "localized-root", name+".html"))
			if err != nil {
				t.Fatal(err)
			}
			got, err := ParseRootPage(b)
			if err != nil {
				t.Fatal(err)
			}
			if got.Version != "16.0.0" || got.Image != "release-tasmota" || got.Minimal || !got.LangKnown || got.Lang != strings.ToLower(name[:2]) {
				t.Errorf("got %+v", got)
			}
		})
	}
}

// The localized minimal and safeboot pages translate the warning (D_MINIMAL_FIRMWARE_PLEASE_UPGRADE);
// the image name in the footer ("minimal") does not change, so it decides for minimal firmware.
// D_SAFEBOOT is translated in a few languages (it_IT "BOOT SICURO", pl_PL "Bezpieczny rozruch",
// lt_LT "SAUGUS PALEIDIMAS"), but the official safeboot images are built with the English heading,
// so the heading text is only a second witness. Minimal firmware must stay recognised on a
// localized build, or the recovery path is lost.
func TestParseRootPage_LocalizedMinimalAndSafeboot(t *testing.T) {
	for _, c := range []struct{ name, warn, by string }{
		{"fr_FR", "Firmware MINIMAL<br>merci de mettre à jour", "par"},
		{"de_DE", "MINIMAL-Firmware<br>Upgrade erforderlich", "von"},
		{"zh_CN", "当前是精简版固件<br>请升级", "作者:"},
	} {
		t.Run(c.name, func(t *testing.T) {
			page := strings.NewReplacer("MINIMAL firmware<br>please upgrade", c.warn, " by Theo", " "+c.by+" Theo").Replace(minimalRoot)
			got, err := ParseRootPage([]byte(page))
			if err != nil || !got.Minimal || got.Image != "release-minimal" {
				t.Errorf("minimal: %+v, %v", got, err)
			}
			safeboot := strings.NewReplacer("MINIMAL firmware<br>please upgrade", "SAFEBOOT", "release-minimal", "release-tasmota32", " by Theo", " "+c.by+" Theo").Replace(minimalRoot)
			got, err = ParseRootPage([]byte(safeboot))
			if err != nil || !got.Minimal {
				t.Errorf("safeboot: %+v, %v", got, err)
			}
		})
	}
}

// #872: a vendor build (MediaRath, a German 14.3.0 on a NOUS A1T) changed Tasmota's HTTP_END: its
// footer drops the "(<image>)" and adds a "Flashed by" line. The device was refused before any
// variant decision ("no Tasmota footer on main page"), and the language the page carries was lost
// with it. Why it matters: on full firmware the page is read only for the language build; the
// image and variant come from Status 2. A footer that keeps Tasmota's credit must still be read,
// with the image left empty (never guessed), and nothing after the credit may change the result.
func TestParseRootPage_VendorFooterWithoutImage(t *testing.T) {
	footer, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "vendor-footer", "mediarath-14.3.0-de.html"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseRootPage(footer)
	if err != nil {
		t.Fatalf("real vendor footer: %v", err)
	}
	if got.Version != "14.3.0" || got.Image != "" || got.LangKnown || got.Minimal {
		t.Errorf("real vendor footer alone: %+v", got)
	}

	// The same footer in place of the stock one on a German main page: the language is read.
	page, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "localized-root", "de_DE.html"))
	if err != nil {
		t.Fatal(err)
	}
	const stock = `<div style='text-align:right;font-size:11px;'><hr><a href='https://github.com/arendst/Tasmota' target='_blank' style='color:#aaa;'>Tasmota 16.0.0 (release-tasmota) von Theo Arends</a></div></div>`
	if strings.Count(string(page), stock) != 1 {
		t.Fatal("de_DE.html no longer carries the stock footer this test replaces")
	}
	got, err = ParseRootPage([]byte(strings.Replace(string(page), stock, string(footer), 1)))
	if err != nil {
		t.Fatalf("German page with the vendor footer: %v", err)
	}
	if got.Version != "14.3.0" || got.Image != "" || got.Lang != "de" || !got.LangKnown || got.Minimal {
		t.Errorf("German page with the vendor footer: %+v", got)
	}
}

// Why: dropping the English "by" (and, #872, accepting a footer without the image) must not turn
// any page that mentions Tasmota into a Tasmota device. The credit stays strict: "Theo Arends"
// close behind the version, no markup in between, nothing in front of the version. The footer
// image is trusted only on minimal and safeboot firmware, where fwselect refuses an empty one, so
// a footer without it is read with Image "" instead of being refused.
func TestParseRootPage_RefusesPagesThatAreNotTheTasmotaFooter(t *testing.T) {
	for _, page := range []string{"Tasmota 15.1.0 by Theo Arends", "Tasmota 15.1.0 release-tasmota by Theo Arends"} {
		got, err := ParseRootPage([]byte(page))
		if err != nil || got.Version != "15.1.0" || got.Image != "" {
			t.Errorf("footer without image in parentheses %q: %+v, %v", page, got, err)
		}
	}
	for name, page := range map[string]string{
		"markup between image and credit":   "Tasmota 15.1.0 (release-tasmota) <b>hello</b> Theo Arends",
		"markup between version and credit": "Tasmota 15.1.0 <b>hello</b> Theo Arends",
		"markup in the image":               "Tasmota 15.1.0 (release-<b>tasmota</b>) by Theo Arends",
		"credit far from the image":         "Tasmota 15.1.0 (release-tasmota) " + strings.Repeat("blah ", 20) + "Theo Arends",
		"credit far from the version":       "Tasmota 15.1.0 " + strings.Repeat("blah ", 20) + "Theo Arends",
		"no credit":                         "Tasmota 15.1.0 (release-tasmota) by someone else",
		"other project":                     "Sonoff 15.1.0 (release-tasmota) by Theo Arends",
		"credit before the version":         "Theo Arends Tasmota 15.1.0 (release-tasmota)",
		"41 characters before the credit":   "Tasmota 15.1.0 (release-tasmota) " + strings.Repeat("x", 41) + "Theo Arends",
	} {
		if _, err := ParseRootPage([]byte(page)); !errors.Is(err, ErrBadResponse) {
			t.Errorf("%s: err = %v, want ErrBadResponse", name, err)
		}
	}
}

// Why (#468): the firmware upload is the update path for devices that take a file. Its reply page
// says "Successful"/"Failed" in the device's language (D_SUCCESSFUL, D_FAILED), so the client reads
// the language-free parts instead: the coloured heading, the reload script only success writes,
// and the reason after a failure. A German build must flash like an English one, and its refusal
// must stay a certain refusal (nothing flashed), not an unknown outcome the caller has to probe.
func TestUploadFirmwareReadsLocalizedReplies(t *testing.T) {
	for _, c := range []struct {
		name, reply string
		wantErr     error
		wantReason  string
	}{
		{"de success", `<script>setTimeout(function(){location.href='.';},10000);</script><b>Hochladen <font color='#008000'>erfolgreich</font></b><br><br><div>Das Gerät wird neu gestartet</div>`, nil, ""},
		{"fr success", `<script>setTimeout(function(){location.href='.';},10000);</script><b>Téléversement <font color='#008000'>Réussi</font></b><br><br><div>Mise à jour terminée</div>`, nil, ""},
		{"de failure", `<b>Hochladen <font color='#ff5661'>fehlgeschlagen</font></b><br><br>Datei ungültig</div><br>`, ErrUploadRefused, "Datei ungültig"},
		{"unknown page", `<html>something else</html>`, ErrBadResponse, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = w.Write([]byte("<form method='post' action='u2?fsz=' enctype='multipart/form-data'><input type='file' name='u2'></form>"))
					return
				}
				_, _ = io.Copy(io.Discard, r.Body)
				_, _ = w.Write([]byte(c.reply))
			}))
			defer srv.Close()
			err := upload(uploader(t, Options{}), target(t, srv, 1, "", ""), []byte{0xE9, 1, 2, 3})
			if c.wantErr == nil && err != nil {
				t.Fatalf("got %v", err)
			}
			if c.wantErr != nil && (!errors.Is(err, c.wantErr) || !strings.Contains(err.Error(), c.wantReason)) {
				t.Fatalf("got %v, want %v with %q", err, c.wantErr, c.wantReason)
			}
		})
	}
}

// Why: the gap between image and credit is bounded (40 characters), and the bound is exact: a longer
// word would admit prose around the credit, a shorter one would refuse a real translation.
func TestParseRootPage_FooterGapIsExactly40(t *testing.T) {
	page := func(n int) string {
		return "Tasmota 15.1.0 (release-tasmota) " + strings.Repeat("x", n) + "Theo Arends"
	}
	if _, err := ParseRootPage([]byte(page(40))); err != nil {
		t.Errorf("40 characters: %v", err)
	}
	if _, err := ParseRootPage([]byte(page(41))); !errors.Is(err, ErrBadResponse) {
		t.Errorf("41 characters: err = %v, want ErrBadResponse", err)
	}
	// The same bound behind the version when a vendor footer has no image (#872).
	noImage := func(n int) string { return "Tasmota 15.1.0 " + strings.Repeat("x", n) + "Theo Arends" }
	if _, err := ParseRootPage([]byte(noImage(40))); err != nil {
		t.Errorf("40 characters without image: %v", err)
	}
	if _, err := ParseRootPage([]byte(noImage(41))); !errors.Is(err, ErrBadResponse) {
		t.Errorf("41 characters without image: err = %v, want ErrBadResponse", err)
	}
}

// Why: success is the heading AND the reload script of HandleUploadDone. The script alone is not a
// success (a reply could carry it without the upload having been accepted), and the Zigbee flasher's
// page (/u3) has the heading with another redirect: a different flow, never a firmware success.
func TestUploadReplyNeedsHeadingAndTheExactReloadScript(t *testing.T) {
	script := `<script>setTimeout(function(){location.href='.';},10000);</script>`
	if err := uploadReply(script + `<html>no heading</html>`); !errors.Is(err, ErrBadResponse) {
		t.Errorf("script without heading: %v", err)
	}
	zigbee := `<script>setTimeout(function(){location.href='/u3';},10000);</script><b>Upload <font color='#008000'>Successful</font></b>`
	if err := uploadReply(zigbee); err == nil {
		t.Error("the Zigbee flasher page was read as a firmware upload success")
	}
}
