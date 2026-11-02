package fwselect

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// upstreamLanguage is one tasmota/language/<File>.h of arendst/Tasmota development
// (D_HTML_LANGUAGE) with the suffix of its release build (MY_LANGUAGE of [env:tasmota-<Suffix>] in
// platformio_tasmota_env.ini). Read through the GitHub API on 2026-11-02, not derived from each
// other: that the two differ is the whole point.
type upstreamLanguage struct {
	File   string
	HTML   string // exactly as the header writes it
	Suffix string // "" for en_GB (the plain build)
	Name   string // the name the language choice shows; "" for English
	LCID   string // LANGUAGE_LCID of the header in hex: the first entry of Features in Status 4 ("%04X")
}

var upstreamLanguages = []upstreamLanguage{
	{"af_AF", "af", "AF", "Afrikaans", "0436"},
	{"bg_BG", "bg", "BG", "Bulgarian", "0402"},
	{"ca_AD", "ca", "AD", "Catalan", "0403"},
	{"cs_CZ", "cs", "CZ", "Czech", "0405"},
	{"de_DE", "de", "DE", "German", "0407"},
	{"el_GR", "el", "GR", "Greek", "0408"},
	{"en_GB", "en", "", "", "0809"},
	{"es_ES", "es", "ES", "Spanish", "040A"},
	{"fr_FR", "fr", "FR", "French", "040C"},
	{"fy_NL", "fy", "FY", "Frisian", "0462"},
	{"he_HE", "he", "HE", "Hebrew", "040D"},
	{"hu_HU", "hu", "HU", "Hungarian", "040E"},
	{"it_IT", "it", "IT", "Italian", "0410"},
	{"ko_KO", "ko", "KO", "Korean", "0412"},
	{"lt_LT", "lt", "LT", "Lithuanian", "0427"},
	{"nl_NL", "nl", "NL", "Dutch", "0413"},
	{"pl_PL", "pl", "PL", "Polish", "0415"},
	{"pt_BR", "pt", "BR", "Portuguese (Brazil)", "0416"},
	{"pt_PT", "pt", "PT", "Portuguese (Portugal)", "0816"},
	{"ro_RO", "ro", "RO", "Romanian", "0818"},
	{"ru_RU", "ru", "RU", "Russian", "0419"},
	{"sk_SK", "sk", "SK", "Slovak", "041B"},
	{"sv_SE", "sv", "SE", "Swedish", "041D"},
	{"tr_TR", "tr", "TR", "Turkish", "041F"},
	{"uk_UA", "uk", "UK", "Ukrainian", "0422"},
	{"vi_VN", "vi", "VN", "Vietnamese", "042A"},
	{"zh_CN", "zh", "CN", "Chinese (simplified)", "0804"},
	{"zh_TW", "zh-Hant-TW", "TW", "Chinese (traditional)", "0404"},
}

// status4With is the Status 4 reply of a captured device (a bedroom-cabinet fixture) whose Features
// start with the given locale ID, the way Tasmota writes it (support_features.ino:
// ResponseAppendFeatures puts LANGUAGE_LCID as "%04X" first).
func status4With(t *testing.T, lcid string) []byte {
	t.Helper()
	raw := readFixture(t, "devices/bedroom-cabinet/status-4.json")
	if !strings.Contains(string(raw), `"Features":["0809",`) {
		t.Fatal("fixture test data: bedroom-cabinet Status 4 no longer starts its Features with 0809")
	}
	return []byte(strings.Replace(string(raw), `"Features":["0809",`, `"Features":["`+lcid+`",`, 1))
}

// Why (#509): the page says the ISO code, the release file has its own suffix. Upper-casing the
// code asked for tasmota-CS.bin.gz (no such file: the Czech device could not be updated) and
// took tasmota-PT for a Brazilian device (it came back in European Portuguese). Every upstream
// language file must lead to the file of its own build, from a device page to the file name, on
// both chip lines. The one code two builds share ("pt") is told apart by the locale ID in
// Status 4; without it the device must not lead anywhere, so the person is asked.
func TestEveryUpstreamLanguageLeadsToItsOwnBuild(t *testing.T) {
	ambiguous := map[string]bool{}
	for _, u := range upstreamLanguages {
		for _, v := range upstreamLanguages {
			if u.File != v.File && u.HTML == v.HTML {
				ambiguous[u.HTML] = true
			}
		}
	}
	if !ambiguous["pt"] || len(ambiguous) != 1 {
		t.Fatalf("test data: only pt is shared by two builds, got %v", ambiguous)
	}
	for _, u := range upstreamLanguages {
		t.Run(u.File, func(t *testing.T) {
			root := &RootInfo{Lang: u.HTML, LangKnown: true}
			dash := ""
			if u.Suffix != "" {
				dash = "-" + u.Suffix
			}
			for _, hw := range []struct{ image, hardware, file string }{
				{"release-tasmota", "ESP8266EX", "tasmota" + dash + ".bin.gz"},
				{"release-tasmota32", "ESP32-D0WD-V3", "tasmota32" + dash + ".bin"},
			} {
				s2 := status2("16.0.0("+hw.image+")", hw.hardware)
				// With the device's own Status 4 every build is known and leads to its own file.
				id, err := Detect(Inputs{Status2: s2, Status4: status4With(t, u.LCID), Root: root})
				wantErr(t, err, nil)
				if !id.LanguageKnown || id.Language != u.Suffix {
					t.Fatalf("%s: <html lang=%q> with locale %s read as %q known %v, want %q", hw.hardware, u.HTML, u.LCID, id.Language, id.LanguageKnown, u.Suffix)
				}
				sel, err := Select(id, Choice{Release: "16.0.0"})
				wantErr(t, err, nil)
				if sel.Filename != hw.file {
					t.Fatalf("%s: <html lang=%q> selects %q, want %q", hw.hardware, u.HTML, sel.Filename, hw.file)
				}
				// Without Status 4 only the page is left: everything but "pt" is still known.
				id, err = Detect(Inputs{Status2: s2, Root: root})
				wantErr(t, err, nil)
				if !ambiguous[u.HTML] {
					if !id.LanguageKnown || id.Language != u.Suffix {
						t.Fatalf("%s: <html lang=%q> alone read as %q known %v, want %q", hw.hardware, u.HTML, id.Language, id.LanguageKnown, u.Suffix)
					}
					continue
				}
				if id.LanguageKnown || id.Language != "" {
					t.Fatalf("%s: language %q known %v, want unknown: the page of tasmota-BR and tasmota-PT is the same", hw.hardware, id.Language, id.LanguageKnown)
				}
				if _, err := Select(id, Choice{Release: "16.0.0"}); !errors.Is(err, ErrLanguageUnknown) {
					t.Fatalf("%s: Select without a choice = %v, want ErrLanguageUnknown", hw.hardware, err)
				}
				// The person's answer is what picks the file.
				sel, err = Select(id, Choice{Release: "16.0.0", Language: u.Suffix, AllowVariantChange: true})
				wantErr(t, err, nil)
				if sel.Filename != hw.file {
					t.Fatalf("%s: chosen file %q, want %q", hw.hardware, sel.Filename, hw.file)
				}
			}
		})
	}
}

// Why (#509): "pt" is only settled by a locale ID that belongs to a Portuguese build. A Status 4
// without Features, with another locale, or in a shape we do not know must leave the device
// unread and asked, never turn into BR or PT; and an odd Features must not cost the device its
// identity (Status 4 is also where the flash sizes come from).
func TestPortuguesePageNeedsALocaleOfAPortugueseBuild(t *testing.T) {
	s2 := status2("16.0.0(release-tasmota)", "ESP8266EX")
	mem := func(features string) []byte {
		return []byte(`{"StatusMEM":{"ProgramSize":651,"Free":352,"ProgramFlashSize":1024,"FlashSize":1024` + features + `}}`)
	}
	tests := []struct {
		name   string
		status []byte
		want   string
		known  bool
	}{
		{"brazil", mem(`,"Features":["0416","8F9AC787"]`), "BR", true},
		{"portugal", mem(`,"Features":["0816","8F9AC787"]`), "PT", true},
		{"no Features", mem(``), "", false},
		{"empty Features", mem(`,"Features":[]`), "", false},
		{"empty locale", mem(`,"Features":[""]`), "", false},
		{"english locale", mem(`,"Features":["0809","8F9AC787"]`), "", false},
		{"czech locale with a pt page", mem(`,"Features":["0405"]`), "", false},
		{"not hex", mem(`,"Features":["ZZZZ"]`), "", false},
		{"locale only in a later entry", mem(`,"Features":["8F9AC787","0416"]`), "", false},
		{"Features is a number", mem(`,"Features":5`), "", false},
		{"Features holds numbers", mem(`,"Features":[1046]`), "", false},
		{"Features is null", mem(`,"Features":null`), "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := Detect(Inputs{Status2: s2, Status4: tt.status, Root: &RootInfo{Lang: "pt", LangKnown: true}})
			wantErr(t, err, nil)
			if id.Language != tt.want || id.LanguageKnown != tt.known {
				t.Fatalf("language %q known %v, want %q known %v", id.Language, id.LanguageKnown, tt.want, tt.known)
			}
			if id.FlashSizeKB != 1024 || id.FreeKB != 352 {
				t.Fatalf("the flash numbers of Status 4 were lost: %+v", id)
			}
		})
	}
}

// The locale only settles a page code that two builds write. For every other build the page is
// enough, as before, whatever Status 4 says; and a locale cannot turn a page that is not
// Portuguese into a Portuguese build.
func TestLocaleIDDoesNotOverrideAClearPage(t *testing.T) {
	s2 := status2("16.0.0(release-tasmota)", "ESP8266EX")
	for _, tt := range []struct{ lang, lcid, want string }{
		{"de", "0416", "DE"},
		{"cs", "0816", "CZ"},
		{"en", "0416", ""},
	} {
		id, err := Detect(Inputs{Status2: s2, Status4: status4With(t, tt.lcid), Root: &RootInfo{Lang: tt.lang, LangKnown: true}})
		wantErr(t, err, nil)
		if !id.LanguageKnown || id.Language != tt.want {
			t.Errorf("<html lang=%q> with locale %s read as %q known %v, want %q", tt.lang, tt.lcid, id.Language, id.LanguageKnown, tt.want)
		}
	}
	// Not read from a device page (MQTT-only): the locale alone is no language, the person is asked.
	id, err := Detect(Inputs{Status2: s2, Status4: status4With(t, "0416"), Root: &RootInfo{Lang: "pt"}})
	wantErr(t, err, nil)
	if id.LanguageKnown {
		t.Errorf("a page that was not read made the language known: %q", id.Language)
	}
}

// Why (D10, #509): a code that is not in the table is not a language build. A custom build or a
// language upstream adds later must be asked for, never turned into a file name that was made up
// from the code. The old upper-casing gave "ZH-HANT-TW" or "XX" here.
func TestUnlistedHTMLLanguageIsUnknownNotGuessed(t *testing.T) {
	for _, lang := range []string{"xx", "zh-Hant", "en-GB", "pt-BR", "deu", "d", "tasmota-DE", "xé", "   "} {
		root := &RootInfo{Lang: lang, LangKnown: true}
		id, err := Detect(Inputs{Status2: status2("16.0.0(release-tasmota)", "ESP8266EX"), Root: root})
		wantErr(t, err, nil)
		if id.LanguageKnown || id.Language != "" {
			t.Errorf("<html lang=%q> read as %q known %v, want unknown", lang, id.Language, id.LanguageKnown)
		}
	}
}

// Case and blanks around the code do not matter: "DE" or " de " is German.
func TestHTMLLanguageIgnoresCaseAndBlanks(t *testing.T) {
	for in, want := range map[string]string{"DE": "DE", " de ": "DE", "Zh-hant-tw": "TW", "EN": "", " en": ""} {
		got, ok := languageFromHTML(in, "")
		if !ok || got != want {
			t.Errorf("languageFromHTML(%q) = %q, %v, want %q, true", in, got, ok, want)
		}
	}
}

// The language choice (Languages) and the table behind the detection are one list: every build
// the upstream release has is offered, each exactly once, and nothing else. Lithuanian was
// missing from the choice although tasmota-LT exists.
func TestLanguageChoiceListsEveryUpstreamBuildOnce(t *testing.T) {
	var want, got []string
	names, locales := map[string]string{}, map[string]string{}
	for _, u := range upstreamLanguages {
		if u.Suffix != "" {
			want = append(want, u.Suffix)
			names[u.Suffix], locales[u.Suffix] = u.Name, u.LCID
		}
	}
	seen := map[string]bool{}
	for _, l := range Languages() {
		got = append(got, l.Suffix)
		if l.Name != names[l.Suffix] {
			t.Errorf("%s is named %q, want %q", l.Suffix, l.Name, names[l.Suffix])
		}
		if l.LCID != locales[l.Suffix] || seen[l.LCID] {
			t.Errorf("%s has locale %q (upstream %q, or used twice): the locale tells builds with the same page code apart", l.Suffix, l.LCID, locales[l.Suffix])
		}
		seen[l.LCID] = true
		if !isLanguage(l.Suffix) || l.Suffix != strings.ToUpper(l.Suffix) {
			t.Errorf("%q is not a language suffix", l.Suffix)
		}
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("offered builds\n got %v\nwant %v", got, want)
	}
	if len(got) != 27 {
		t.Fatalf("%d language builds, upstream has 27 besides English", len(got))
	}
}

// Languages hands out a copy: a caller that sorts or edits the list must not change detection.
func TestLanguagesReturnsACopy(t *testing.T) {
	l := Languages()
	l[0].HTML, l[0].Suffix = "de", "XX"
	if got, ok := languageFromHTML("de", ""); !ok || got != "DE" {
		t.Fatalf("languageFromHTML(de) = %q, %v after the copy was edited", got, ok)
	}
	if again := Languages(); again[0].Suffix == "XX" {
		t.Fatal("Languages returned the table itself")
	}
}
