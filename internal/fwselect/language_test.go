package fwselect

import (
	"errors"
	"slices"
	"strconv"
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

// Why (#686, #509): the language build decides the file a device gets, and a German device must not
// come back in English. Status 4 carries the build's locale ID over HTTP and over MQTT alike, and every
// upstream build has its own, so it alone names the build: no main page is read for it (the page is
// HTML for people and may change with the firmware, #616). Every upstream language file must lead from
// its locale to the file of its own build, on both chip lines, with the locale written the way current
// firmware prints it ("%04X") and zero-padded to eight digits. The page has no say: a page that names
// another language (or none) does not change what the locale says.
func TestTheLanguageBuildComesFromTheStatus4LocaleIDAlone(t *testing.T) {
	for _, u := range upstreamLanguages {
		t.Run(u.File, func(t *testing.T) {
			dash := ""
			if u.Suffix != "" {
				dash = "-" + u.Suffix
			}
			for _, lcid := range []string{u.LCID, "0000" + u.LCID, strings.ToLower(u.LCID)} {
				for _, root := range []*RootInfo{nil, {Lang: "xx", LangKnown: true}, {Lang: "en", LangKnown: true}} {
					for _, hw := range []struct{ image, hardware, file string }{
						{"release-tasmota", "ESP8266EX", "tasmota" + dash + ".bin.gz"},
						{"release-tasmota32", "ESP32-D0WD-V3", "tasmota32" + dash + ".bin"},
					} {
						id, err := Detect(Inputs{Status2: status2("16.0.0("+hw.image+")", hw.hardware), Status4: status4With(t, lcid), Root: root})
						wantErr(t, err, nil)
						if !id.LanguageKnown || id.Language != u.Suffix {
							t.Fatalf("%s: locale %s (page %+v) read as %q known %v, want %q", hw.hardware, lcid, root, id.Language, id.LanguageKnown, u.Suffix)
						}
						sel, err := Select(id, Choice{Release: "16.0.0"})
						wantErr(t, err, nil)
						if sel.Filename != hw.file {
							t.Fatalf("%s: locale %s selects %q, want %q", hw.hardware, lcid, sel.Filename, hw.file)
						}
					}
				}
			}
		})
	}
}

// Why (D10, #686): a locale ID that names no build of the table (a self-compiled build with another
// MY_LANGUAGE, a Features list in a shape we do not know) must leave the device's language unread, so
// the person is asked; a main page that says German must not fill it in, because the page is no longer
// a source of the language. An odd Features must not cost the device its identity either (Status 4 is
// also where the flash sizes come from). Without Status 4 at all the language is unread too.
func TestAnUnknownLocaleIDLeavesTheLanguageUnreadWhateverThePageSays(t *testing.T) {
	s2 := status2("16.0.0(release-tasmota)", "ESP8266EX")
	mem := func(features string) []byte {
		return []byte(`{"StatusMEM":{"ProgramSize":651,"Free":352,"ProgramFlashSize":1024,"FlashSize":1024` + features + `}}`)
	}
	tests := []struct {
		name   string
		status []byte
	}{
		{"a locale of no build", mem(`,"Features":["ABCD","8F9AC787"]`)},
		{"no Features", mem(``)},
		{"empty Features", mem(`,"Features":[]`)},
		{"empty locale", mem(`,"Features":[""]`)},
		{"not hex", mem(`,"Features":["ZZZZ"]`)},
		{"a feature bitmap first", mem(`,"Features":["8F9AC787","0407"]`)},
		{"signed", mem(`,"Features":["+0407"]`)},
		{"hex prefix", mem(`,"Features":["0x0407"]`)},
		{"longer than 32 bits", mem(`,"Features":["100000407"]`)},
		{"Features is a number", mem(`,"Features":5`)},
		{"Features holds numbers", mem(`,"Features":[1031]`)},
		{"Features is null", mem(`,"Features":null`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := Detect(Inputs{Status2: s2, Status4: tt.status, Root: &RootInfo{Lang: "de", LangKnown: true}})
			wantErr(t, err, nil)
			if id.Language != "" || id.LanguageKnown {
				t.Fatalf("language %q known %v, want unread", id.Language, id.LanguageKnown)
			}
			if id.FlashSizeKB != 1024 || id.FreeKB != 352 {
				t.Fatalf("the flash numbers of Status 4 were lost: %+v", id)
			}
			if _, err := Select(id, Choice{Release: "16.0.0"}); !errors.Is(err, ErrLanguageUnknown) {
				t.Fatalf("Select without a choice = %v, want ErrLanguageUnknown", err)
			}
		})
	}
	id, err := Detect(Inputs{Status2: s2, Root: &RootInfo{Lang: "de", LangKnown: true}})
	wantErr(t, err, nil)
	if id.LanguageKnown || id.Language != "" {
		t.Fatalf("without Status 4 the page made the language %q known %v", id.Language, id.LanguageKnown)
	}
}

// Why (#686): the locale ID alone names the build, so no two builds may share one, and none may share
// the English build's 0809 (it has no row of its own: its file has no suffix). Locales are compared by
// value, so the check is by value too.
func TestEveryLocaleIDNamesOneBuild(t *testing.T) {
	seen := map[uint64]string{lcidEnglish: "English"}
	for _, l := range languages {
		v, err := strconv.ParseUint(l.LCID, 16, 32)
		if err != nil {
			t.Fatalf("%s: locale %q is not hex", l.Suffix, l.LCID)
		}
		if other, ok := seen[v]; ok {
			t.Fatalf("%s and %s share the locale %s", l.Suffix, other, l.LCID)
		}
		seen[v] = l.Suffix
	}
}

// Why (D10, #509): on the main page of minimal and safeboot firmware (the only page still read for a
// language), a code that is not in the table is not a language build. A custom build or a language
// upstream adds later must be asked for, never turned into a file name that was made up from the code.
// "pt" is written by two builds and names neither without the locale, which that firmware cannot report.
func TestUnlistedHTMLLanguageIsUnknownNotGuessed(t *testing.T) {
	for _, lang := range []string{"xx", "zh-Hant", "en-GB", "pt-BR", "pt", "deu", "d", "tasmota-DE", "xé", "   "} {
		if got, ok := languageFromRoot(&RootInfo{Lang: lang, LangKnown: true}); ok || got != "" {
			t.Errorf("<html lang=%q> read as %q known %v, want unknown", lang, got, ok)
		}
	}
}

// Case and blanks around the code do not matter: "DE" or " de " is German.
func TestHTMLLanguageIgnoresCaseAndBlanks(t *testing.T) {
	for in, want := range map[string]string{"DE": "DE", " de ": "DE", "Zh-hant-tw": "TW", "EN": "", " en": ""} {
		got, ok := languageFromHTML(in)
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
	if got, ok := languageFromHTML("de"); !ok || got != "DE" {
		t.Fatalf("languageFromHTML(de) = %q, %v after the copy was edited", got, ok)
	}
	if again := Languages(); again[0].Suffix == "XX" {
		t.Fatal("Languages returned the table itself")
	}
}
