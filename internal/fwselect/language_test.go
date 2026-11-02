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
	Name   string // the name the language choice shows; "" when the page cannot name the build
}

var upstreamLanguages = []upstreamLanguage{
	{"af_AF", "af", "AF", "Afrikaans"},
	{"bg_BG", "bg", "BG", "Bulgarian"},
	{"ca_AD", "ca", "AD", "Catalan"},
	{"cs_CZ", "cs", "CZ", "Czech"},
	{"de_DE", "de", "DE", "German"},
	{"el_GR", "el", "GR", "Greek"},
	{"en_GB", "en", "", ""},
	{"es_ES", "es", "ES", "Spanish"},
	{"fr_FR", "fr", "FR", "French"},
	{"fy_NL", "fy", "FY", "Frisian"},
	{"he_HE", "he", "HE", "Hebrew"},
	{"hu_HU", "hu", "HU", "Hungarian"},
	{"it_IT", "it", "IT", "Italian"},
	{"ko_KO", "ko", "KO", "Korean"},
	{"lt_LT", "lt", "LT", "Lithuanian"},
	{"nl_NL", "nl", "NL", "Dutch"},
	{"pl_PL", "pl", "PL", "Polish"},
	{"pt_BR", "pt", "BR", "Portuguese (Brazil)"},
	{"pt_PT", "pt", "PT", "Portuguese (Portugal)"},
	{"ro_RO", "ro", "RO", "Romanian"},
	{"ru_RU", "ru", "RU", "Russian"},
	{"sk_SK", "sk", "SK", "Slovak"},
	{"sv_SE", "sv", "SE", "Swedish"},
	{"tr_TR", "tr", "TR", "Turkish"},
	{"uk_UA", "uk", "UK", "Ukrainian"},
	{"vi_VN", "vi", "VN", "Vietnamese"},
	{"zh_CN", "zh", "CN", "Chinese (simplified)"},
	{"zh_TW", "zh-Hant-TW", "TW", "Chinese (traditional)"},
}

// Why (#509): the page says the ISO code, the release file has its own suffix. Upper-casing the
// code asked for tasmota-CS.bin.gz (no such file: the Czech device could not be updated) and
// took tasmota-PT for a Brazilian device (it came back in European Portuguese). Every upstream
// language file must lead to the file of its own build, from a device page to the file name, on
// both chip lines; the one code two builds share must not lead anywhere, so the person is asked.
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
				id, err := Detect(Inputs{Status2: status2("16.0.0("+hw.image+")", hw.hardware), Root: root})
				wantErr(t, err, nil)
				if ambiguous[u.HTML] {
					if id.LanguageKnown || id.Language != "" {
						t.Fatalf("%s: language %q known %v, want unknown: the page of tasmota-BR and tasmota-PT is the same", hw.hardware, id.Language, id.LanguageKnown)
					}
					if _, err := Select(id, Choice{Release: "16.0.0"}); !errors.Is(err, ErrLanguageUnknown) {
						t.Fatalf("%s: Select without a choice = %v, want ErrLanguageUnknown", hw.hardware, err)
					}
					// The person's answer is what picks the file.
					sel, err := Select(id, Choice{Release: "16.0.0", Language: u.Suffix, AllowVariantChange: true})
					wantErr(t, err, nil)
					if sel.Filename != hw.file {
						t.Fatalf("%s: chosen file %q, want %q", hw.hardware, sel.Filename, hw.file)
					}
					continue
				}
				if !id.LanguageKnown || id.Language != u.Suffix {
					t.Fatalf("%s: <html lang=%q> read as %q known %v, want %q", hw.hardware, u.HTML, id.Language, id.LanguageKnown, u.Suffix)
				}
				sel, err := Select(id, Choice{Release: "16.0.0"})
				wantErr(t, err, nil)
				if sel.Filename != hw.file {
					t.Fatalf("%s: <html lang=%q> selects %q, want %q", hw.hardware, u.HTML, sel.Filename, hw.file)
				}
			}
		})
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
	names := map[string]string{}
	for _, u := range upstreamLanguages {
		if u.Suffix != "" {
			want = append(want, u.Suffix)
			names[u.Suffix] = u.Name
		}
	}
	for _, l := range Languages() {
		got = append(got, l.Suffix)
		if l.Name != names[l.Suffix] {
			t.Errorf("%s is named %q, want %q", l.Suffix, l.Name, names[l.Suffix])
		}
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
