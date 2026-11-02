package fwselect

import "strings"

// Language is one language build of upstream Tasmota. The release file is
// tasmota-<Suffix>.bin.gz (tasmota32-<Suffix>.bin on a dual-core ESP32); the build itself writes
// HTML into <html lang> of its main page (D_HTML_LANGUAGE in tasmota/language/<lang>.h).
// The two are unrelated spellings: Czech is built as "CZ" (cs_CZ.h) but its page says "cs".
type Language struct {
	Suffix string // upstream release suffix, e.g. "CZ"; the [env:tasmota-<Suffix>] of platformio_tasmota_env.ini
	HTML   string // D_HTML_LANGUAGE of the build, lower case; two builds may write the same code
	Name   string // English name for people
	LCID   string // LANGUAGE_LCID of the header in hex ("%04X"), the first entry of Features in Status 4
}

// languages are the 27 language builds besides English (tasmota/language/*.h with the
// MY_LANGUAGE of each [env:tasmota-XX] in platformio_tasmota_env.ini, checked against
// arendst/Tasmota development on 2026-11-02). The order is the order people see.
//
// pt_BR.h and pt_PT.h both write "pt": the page cannot tell BR from PT. Status 4 can: Tasmota
// writes the locale ID of the build as the first entry of Features (support_features.ino,
// ResponseAppendFeatures; "0416" for pt_BR, "0816" for pt_PT). languageFromHTML uses it when, and
// only when, two builds share a page code. A new upstream language is added here, with its row in
// the test.
var languages = []Language{
	{"AF", "af", "Afrikaans", "0436"},
	{"AD", "ca", "Catalan", "0403"},
	{"BG", "bg", "Bulgarian", "0402"},
	{"BR", "pt", "Portuguese (Brazil)", "0416"},
	{"CN", "zh", "Chinese (simplified)", "0804"},
	{"TW", "zh-hant-tw", "Chinese (traditional)", "0404"},
	{"CZ", "cs", "Czech", "0405"},
	{"NL", "nl", "Dutch", "0413"},
	{"FR", "fr", "French", "040C"},
	{"FY", "fy", "Frisian", "0462"},
	{"DE", "de", "German", "0407"},
	{"GR", "el", "Greek", "0408"},
	{"HE", "he", "Hebrew", "040D"},
	{"HU", "hu", "Hungarian", "040E"},
	{"IT", "it", "Italian", "0410"},
	{"KO", "ko", "Korean", "0412"},
	{"LT", "lt", "Lithuanian", "0427"},
	{"PL", "pl", "Polish", "0415"},
	{"PT", "pt", "Portuguese (Portugal)", "0816"},
	{"RO", "ro", "Romanian", "0818"},
	{"RU", "ru", "Russian", "0419"},
	{"SK", "sk", "Slovak", "041B"},
	{"ES", "es", "Spanish", "040A"},
	{"SE", "sv", "Swedish", "041D"},
	{"TR", "tr", "Turkish", "041F"},
	{"UK", "uk", "Ukrainian", "0422"},
	{"VN", "vi", "Vietnamese", "042A"},
}

// Languages returns the upstream language builds a person can choose besides English
// (LanguageEnglish), in display order.
func Languages() []Language { return append([]Language(nil), languages...) }

// languageFromHTML maps the <html lang> of a device main page to the upstream suffix of its
// build: "" is the English build. lcid is the locale ID from Status 4 ("" if not read); it only
// decides between builds that write the same page code. ok is false when this does not name one
// build: an empty or unlisted code (a custom build, a language added upstream after this table),
// and a code two builds share without a locale of one of them. Nothing is guessed (D10); the
// caller asks the person.
func languageFromHTML(lang, lcid string) (suffix string, ok bool) {
	l := strings.ToLower(strings.TrimSpace(lang))
	if l == "en" {
		return "", true
	}
	var same []Language
	for _, e := range languages {
		if e.HTML == l {
			same = append(same, e)
		}
	}
	if len(same) == 1 {
		return same[0].Suffix, true
	}
	for _, e := range same {
		if e.LCID == lcid {
			return e.Suffix, true
		}
	}
	return "", false
}
