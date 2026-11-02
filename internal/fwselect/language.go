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
}

// languages are the 27 language builds besides English (tasmota/language/*.h with the
// MY_LANGUAGE of each [env:tasmota-XX] in platformio_tasmota_env.ini, checked against
// arendst/Tasmota development on 2026-11-02). The order is the order people see.
//
// pt_BR.h and pt_PT.h both write "pt": the page cannot tell BR from PT, so languageFromHTML
// answers "unknown" for it. A new upstream language is added here, with its row in the test.
var languages = []Language{
	{"AF", "af", "Afrikaans"},
	{"AD", "ca", "Catalan"},
	{"BG", "bg", "Bulgarian"},
	{"BR", "pt", "Portuguese (Brazil)"},
	{"CN", "zh", "Chinese (simplified)"},
	{"TW", "zh-hant-tw", "Chinese (traditional)"},
	{"CZ", "cs", "Czech"},
	{"NL", "nl", "Dutch"},
	{"FR", "fr", "French"},
	{"FY", "fy", "Frisian"},
	{"DE", "de", "German"},
	{"GR", "el", "Greek"},
	{"HE", "he", "Hebrew"},
	{"HU", "hu", "Hungarian"},
	{"IT", "it", "Italian"},
	{"KO", "ko", "Korean"},
	{"LT", "lt", "Lithuanian"},
	{"PL", "pl", "Polish"},
	{"PT", "pt", "Portuguese (Portugal)"},
	{"RO", "ro", "Romanian"},
	{"RU", "ru", "Russian"},
	{"SK", "sk", "Slovak"},
	{"ES", "es", "Spanish"},
	{"SE", "sv", "Swedish"},
	{"TR", "tr", "Turkish"},
	{"UK", "uk", "Ukrainian"},
	{"VN", "vi", "Vietnamese"},
}

// Languages returns the upstream language builds a person can choose besides English
// (LanguageEnglish), in display order.
func Languages() []Language { return append([]Language(nil), languages...) }

// languageFromHTML maps the <html lang> of a device main page to the upstream suffix of its
// build: "" is the English build. ok is false when the page does not tell the build: an empty or
// unlisted code (a custom build, a language added upstream after this table), and "pt", which
// two builds write. Nothing is guessed (D10); the caller asks the person.
func languageFromHTML(lang string) (suffix string, ok bool) {
	l := strings.ToLower(strings.TrimSpace(lang))
	if l == "en" {
		return "", true
	}
	found := false
	for _, e := range languages {
		if e.HTML != l {
			continue
		}
		if found {
			return "", false // two builds write this code
		}
		suffix, found = e.Suffix, true
	}
	return suffix, found
}
