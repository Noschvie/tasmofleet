package ui

import (
	"context"
	"hash/fnv"
	"html"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
)

// softWrite writes the parts HTML-escaped with a <wbr> between them. Written by hand, not in a
// .templ file: templ puts a space between sibling nodes, and a space is a visible character.
func softWrite(parts []string) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		for i, p := range parts {
			if i > 0 {
				if _, err := io.WriteString(w, "<wbr>"); err != nil {
					return err
				}
			}
			if _, err := io.WriteString(w, html.EscapeString(p)); err != nil {
				return err
			}
		}
		return nil
	})
}

// Soft writes s with break opportunities (<wbr>) at the natural points of a file name, URL or
// host:port (SoftParts), so a narrow column breaks "tasmota-minimal.bin.gz" after "minimal." and
// not inside "bin". The text is the same; overflow-wrap still cuts a token that has no point.
func Soft(s string) templ.Component { return softWrite(SoftParts(s)) }

// SoftJSON writes a device's reply with break opportunities after its commas (SoftJSONParts).
func SoftJSON(s string) templ.Component { return softWrite(SoftJSONParts(s)) }

// SoftJSONParts cuts s after every comma: a device's JSON reply has no spaces, so without these
// points a narrow box would break it inside "Boot" or "ESP8266EX".
func SoftJSONParts(s string) []string {
	parts := strings.SplitAfter(s, ",")
	if n := len(parts); n > 1 && parts[n-1] == "" {
		parts = parts[:n-1]
	}
	return parts
}

// SoftParts cuts s after the characters where a file name, URL or host:port reads naturally across
// two lines: after "/", "-", "_", "|" and "=", after a "." that is followed by a letter (hostnames and
// extensions, not the dots of an IP address or a version), and after a ":" that is followed by a
// digit (the port). A point needs a word before it: a "/", "." or ":" that starts a token stays with
// it ("/login", ".dmp", ":8443"), so a line never ends in the bare character. Joined again the parts are s.
func SoftParts(s string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(s)-1; i++ {
		next := s[i+1]
		isLetter := next >= 'a' && next <= 'z' || next >= 'A' && next <= 'Z'
		isDigit := next >= '0' && next <= '9'
		if (strings.IndexByte("/-_|=", s[i]) >= 0 && next != s[i] || s[i] == '.' && isLetter || s[i] == ':' && isDigit) && wordBefore(s[start:i]) {
			parts = append(parts, s[start:i+1])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}

// wordBefore reports whether the text since the last space holds a letter, a digit or a non-ASCII
// character: only then does a break point after it end a word rather than strand its first character.
func wordBefore(p string) bool {
	if k := strings.LastIndexAny(p, " \t\n"); k >= 0 {
		p = p[k+1:]
	}
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80 {
			return true
		}
	}
	return false
}

// The design system's components (design v2, WP F4; docs/spec/design-system.md, section
// "Components"). Each component takes typed props; values are text, never markup, so templ
// escapes all of them. Sizes that come from data (progress segments, the fleet health bar) are
// SVG presentation attributes, because the content security policy forbids style attributes.

// Tone is the colour family of a badge, dot, banner, toast or tile: ok, warn, danger, neutral or
// accent (information). Anything else renders as neutral.
type Tone string

// The tones of the prototype (TONE in design/v2/template.html).
const (
	ToneOK      Tone = "ok"
	ToneWarn    Tone = "warn"
	ToneDanger  Tone = "danger"
	ToneNeutral Tone = "neutral"
	ToneAccent  Tone = "accent"
)

// class is the tone's CSS class; an unknown tone is neutral, so a typo never drops the colour.
func (t Tone) class() string {
	switch t {
	case ToneOK, ToneWarn, ToneDanger, ToneAccent, ToneNeutral:
		return "t-" + string(t)
	case toneRest: // never drawn
	}
	return "t-neutral"
}

// classOrNone is the tone's class, or no class for no tone at all: a value that carries no state keeps the text colour,
// where class() would draw it neutral.
func (t Tone) classOrNone() string {
	if t == "" {
		return ""
	}
	return t.class()
}

// Btn is a button or a link that looks like one. Kind is "" (default), "primary", "danger"
// (outlined), "danger-solid", "danger-link" (text in the danger colour: a row's Delete), "ghost" or "link" (text
// only). Tall is the large size of a sign-in or start button. With Href it is a link (it only
// navigates, or opens the dialog named by Dialog); without it a button of Type (default
// "button"). Name and Value make it a submit button that sends a value (console chips).
//
// A button that cannot act now sets DisabledReason (#1072): it is drawn aria-disabled (Tab still reaches it) with
// the reason as visible text that aria-describedby names, and it carries no action (blockedButton). Disabled is
// the native attribute, for a button that is off for a moment and says nothing (a request in flight).
type Btn struct {
	Label, Kind, Href, Type string
	Name, Value             string
	Dialog                  string // id of the dialog dialog.js opens instead of following Href
	Rel, Target             string // rel and target of a link (a pager's "prev" and "next"; an external link's "noopener noreferrer" and "_blank")
	Mask                    bool   // text the pixel comparison cannot reproduce (a version): data-visual-mask
	Small, Tall, Disabled   bool   // Small and Tall are the two sizes besides the default: a button is at most one of them
	// ID is the element's id (a link a script or a test finds). Form is the id of the form a submit button belongs to
	// when it stands outside it. FormAction is the address a submit button posts to instead of its form's, and
	// NoValidate skips the form's own validation for that post (a single sign-on button beside a required password).
	ID, Form, FormAction string
	NoValidate           bool
	Current              bool   // the one a row of them is on (a pager's page size): aria-current="page", the accent tint
	Aria                 string // accessible name when the label is a glyph (icon button "⋯")
	Icon                 bool   // square icon button
	// DisabledReason is the visible reason the button cannot act now; it makes the blocked variant, which drops
	// Href, Dialog, Name, Value and Type. ReasonID is the id of the element that holds it: the button's own line
	// (ReasonShared false, drawn under it) or a line the page shows already (ReasonShared true, several buttons
	// may name the same one).
	DisabledReason, ReasonID string
	ReasonShared             bool
	// Class is the page's own class after the button's ("dd-go", "bk-del"): the hook a page's selector or test finds
	// the button by. Attrs are the attributes a script or htmx needs and no typed field covers (hx-*, data-*, title,
	// download, aria-pressed, aria-describedby): see pageAttrs for what is refused. A blocked button drops Attrs, it
	// carries no action. The label may be followed by children (an icon, a count).
	Class string
	Attrs templ.Attributes
	// banner makes the button a banner's action (Banner sets it, no page does): the class banner-btn instead of btn
	// and its kind, size and icon classes, which a banner action has none of.
	banner bool
}

func (b Btn) classes() string {
	if b.banner {
		c := "banner-btn"
		if b.Class != "" {
			c += " " + b.Class
		}
		return c
	}
	c := "btn"
	if b.Kind != "" {
		c += " btn-" + b.Kind
	}
	if b.Small {
		c += " btn-sm"
	}
	if b.Tall {
		c += " btn-tall"
	}
	if b.Icon {
		c += " btn-icon"
	}
	if b.Current {
		c += " btn-current"
	}
	if b.Class != "" {
		c += " " + b.Class
	}
	return c
}

// pageAttrs is what a page may add to a component as Attrs, in a fixed order (templ sorts the keys, so the output
// is the same every time and live.js can memoise a swapped fragment). A key a typed field of the component writes
// already (class, id, href, type, name, value, the form and dialog attributes, the aria names) would appear twice,
// style is blocked by the CSP and on* is inline script: those are dropped, never rendered. An empty result is nil.
func pageAttrs(a templ.Attributes) templ.Attributes {
	var out templ.Attributes
	for k, v := range a {
		key := strings.ToLower(k)
		if pageAttrRefused[key] || strings.HasPrefix(key, "on") {
			continue
		}
		if out == nil {
			out = templ.Attributes{}
		}
		out[k] = v
	}
	return out
}

var pageAttrRefused = map[string]bool{
	"class": true, "style": true, "id": true, "href": true, "type": true, "name": true, "value": true,
	"form": true, "formaction": true, "formnovalidate": true, "disabled": true, "data-dialog": true,
	"target": true, "rel": true, "aria-label": true, "aria-labelledby": true, "aria-current": true,
}

func (b Btn) attrs() templ.Attributes { return pageAttrs(b.Attrs) }

func (b Btn) buttonType() string {
	if b.Type == "" {
		return "button"
	}
	return b.Type
}

// BadgeData is a badge that needs more than a tone and a word: a class of its own (the page's selector), a hint, a
// link, the large size or an id. Every field is text or a flag, never markup, and the output is deterministic
// (live.js memoises swapped fragments as strings). Badge(tone, label) is the short form.
type BadgeData struct {
	Tone  Tone
	Label string
	Class string // classes after the tone's ("dv-upd"); the badge keeps bdg and t-<tone>
	Title string // the hint on hover, for a short label with a longer meaning
	Href  string // makes the badge a link (a running download's progress page)
	Large bool   // bdg-lg: the badge beside a page title
	ID    string // the badge's id, for a live update or a test that finds it
	// Attrs are the attributes a script needs and no field covers (data-push-this, hidden); see pageAttrs.
	Attrs templ.Attributes
}

func (b BadgeData) attrs() templ.Attributes { return pageAttrs(b.Attrs) }

func (b BadgeData) classes() string {
	c := "bdg " + b.Tone.class()
	if b.Large {
		c += " bdg-lg"
	}
	if b.Class != "" {
		c += " " + b.Class
	}
	return c
}

// StatusBadgeData is the connection status of a device (#1097): a tinted badge with a dot and the word, the one look
// of Online, Offline, Stale, Unknown and Auth failed on the device list, its phone cards, the device header and the
// dashboard's attention list. State is the machine word (data-state): the list's script reads it to switch the row's
// actions, so it is the health state itself and never the label. Tone and Label are what the page decided; the
// component has no state table of its own, so the colour and the word cannot drift apart between pages. Title is the
// hint on hover. Everything is text, never markup, and the output is deterministic (live.js memoises swapped fragments).
type StatusBadgeData struct {
	State string
	Tone  Tone
	Label string
	Title string
}

// ChipData is a chip (#1095): a pill that filters or picks. It has three shapes, chosen by which field is set:
//   - Name set: a label around an input (a checkbox, or a radio with Radio), so it works without JavaScript. Value,
//     Checked and Form belong to the input, and so do Attrs (a script finds the control, not the pill).
//   - Href set: a link, a filter that changes the address. Current marks the one the page shows (aria-current="page").
//   - neither: a button for a filter that has no page to link to; Current gives aria-current="true", Attrs carry hx-*.
//
// Label is the text; Count follows it (a string, so a list can show a dash while a count is unknown); Sub is a second
// line (the update quick picks); Long is the full word for a screen reader after an abbreviation ("Mon"); Remove
// adds a cross that shows while a label chip is ticked (a policy tag or exception takes the place of its count).
// Everything is text, never markup. The classes are one set (chip, chip-l, chip-sub, chip-n, chip-x); the selected look is
// the accent tint, drawn by the CSS from the input, aria-current or chip-on.
type ChipData struct {
	Label, Count, Sub, Long, Title string
	Name, Value, Form              string
	Radio, Checked                 bool
	Href                           string
	Current                        bool
	Remove                         bool
	// Attrs are the attributes a script or htmx needs and no field covers (data-*, hx-*); see pageAttrs. A title
	// goes through Title, so Attrs may not carry one.
	Attrs templ.Attributes
}

func (c ChipData) attrs() templ.Attributes { return attrsWithoutTitle(c.Attrs) }

func (c ChipData) inputType() string {
	if c.Radio {
		return "radio"
	}
	return "checkbox"
}

// TagData is a tag that needs more than its words (#1095): a hint, a link (a device in a list), the mono font (an
// address or an id, never a name) or an attribute a script finds it by. Tag(label) is the short form. A tag is never a state.
type TagData struct {
	Label, Title string
	Href         string
	Mono         bool
	Attrs        templ.Attributes // see pageAttrs; a title goes through Title
}

func (t TagData) classes() string {
	if t.Mono {
		return "tag mono"
	}
	return "tag"
}

func (t TagData) attrs() templ.Attributes { return attrsWithoutTitle(t.Attrs) }

// attrsWithoutTitle is pageAttrs for a component that has a Title field: a title in Attrs would be a second one.
func attrsWithoutTitle(a templ.Attributes) templ.Attributes {
	out := pageAttrs(a)
	for k := range out {
		if strings.EqualFold(k, "title") {
			delete(out, k)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ToggleData is a setting switch (#1096): a checkbox with role="switch", 38 x 22. With a Label it is the card of a
// setting (the title, an explaining line, the switch); without one it draws the switch alone, for a setting whose
// label is drawn elsewhere (SettingField, a <label for> the ID). It posts Value when on (the settings form's
// contract is "true", the default) and nothing when off. Everything is text or a flag, never markup.
type ToggleData struct {
	Name, Value, ID string
	Label, Text     string
	On, Disabled    bool
	// Attrs go to the input (aria-describedby, aria-invalid, autofocus, data-*); see pageAttrs. The switch's own
	// attributes (role, aria-checked, checked) are not Attrs' to set.
	Attrs templ.Attributes
}

func (t ToggleData) value() string {
	if t.Value == "" {
		return "true"
	}
	return t.Value
}

func (t ToggleData) attrs() templ.Attributes { return switchAttrs(t.Attrs, false) }

// PowerSwitchData is the relay switch of a device (#1096): 34 x 20 inside a 48 x 44 target, a button with
// role="switch". Exactly one of Label (aria-label) and LabelledBy (aria-labelledby: ids of the device's name and the
// relay's word, so a fragment that goes out for every health event holds no name and no id) names it.
type PowerSwitchData struct {
	Label, LabelledBy string
	On, Disabled      bool
	Submit            bool   // type="submit": the switch of a form that also works without script
	Class             string // classes after pwr, a page's hook (devrelay-sw)
	// Attrs are the attributes htmx and the page need (hx-*, title, aria-describedby); see pageAttrs.
	Attrs templ.Attributes
}

func (p PowerSwitchData) classes() string {
	if p.Class == "" {
		return "pwr"
	}
	return "pwr " + p.Class
}

func (p PowerSwitchData) buttonType() string {
	if p.Submit {
		return "submit"
	}
	return "button"
}

func (p PowerSwitchData) attrs() templ.Attributes { return switchAttrs(p.Attrs, true) }

// switchAttrs is pageAttrs for a switch: role, aria-checked and checked are the component's own (a second one would
// make the switch say two things); a button keeps its title, an input has no use for one.
func switchAttrs(a templ.Attributes, button bool) templ.Attributes {
	out := pageAttrs(a)
	for k := range out {
		switch strings.ToLower(k) {
		case "role", "aria-checked", "checked":
			delete(out, k)
		case "title":
			if !button {
				delete(out, k)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// CopyData is a button that copies text to the clipboard (static/copy.js). Text is the value, From the selector of
// an element whose text is copied instead (a secret shown once must not be in the markup twice); one of them is set.
// The button is hidden in the markup and copy.js shows it, so without JavaScript the page has no dead button.
type CopyData struct {
	Label string // "Copy" when empty
	Text  string
	From  string
	Aria  string // the accessible name when "Copy" alone does not say what is copied
}

func (c CopyData) label() string {
	if c.Label == "" {
		return "Copy"
	}
	return c.Label
}

// BannerData is an inline banner: a dot, a bold lead, the text and at most one action. Rich text (a list, a link, a
// time) follows Text as the templ children of Banner; the action is one Btn, never a second field, because the design
// system allows one (Inline banners): the other answer is a link in the text or a button on the page.
type BannerData struct {
	Tone        Tone
	Title, Text string
	// TextStamp, when its instant is set, is the text instead of Text: a sentence with a <time> in it (Stamped).
	TextStamp Stamped
	// TitleAt, when set, puts a time into the lead: Title, then the instant (TitleAtText is its fallback text, time.js
	// shows it in the browser's zone like a day stamp), then TitleAfter.
	TitleAt     time.Time
	TitleAtText string
	TitleAfter  string
	Action      *Btn
	Alert       bool   // role=alert (a failure the user has to act on) instead of role=status
	ID          string // the banner's id, for a control that aria-describedby points at it (#1072)
	// Class is the page's own class after the tone's ("cmd-check", "name-offer"): the hook a page's selector or test
	// finds the banner by. Attrs are the attributes a script needs and no field covers (data-cooldown); see pageAttrs
	// for what is refused. Pulse makes the dot pulse (a state that is still changing: Reconnecting), Flash marks the
	// banner as the answer to the last action (data-flash), which a page's script or test finds.
	Class string
	Attrs templ.Attributes
	Pulse bool
	Flash bool
}

func (b BannerData) classes() string {
	c := "banner " + b.Tone.class()
	if b.Class != "" {
		c += " " + b.Class
	}
	return c
}

func (b BannerData) attrs() templ.Attributes { return pageAttrs(b.Attrs) }

// action is the banner's action as the button it is rendered with.
func (b BannerData) action() Btn {
	a := *b.Action
	a.banner = true
	return a
}

// The broker ACL notice (#840, #1078): the page that explains the access control list (the long explanation lives
// there only), and the notice's lead, its one sentence and its one action, the same on every page. Without an ACL the
// broker hands what TasmoFleet sends through it to every client that may subscribe.
const (
	BrokerACLDocURL = "https://github.com/hellwand/tasmofleet/blob/main/docs/mqtt-broker-acl.md"
	BrokerACLLead   = "Your MQTT broker has no ACL."
	BrokerACLText   = "Any client on it can read device passwords and backups."
	BrokerACLAction = "Set up the ACL"
)

// ToastData is a toast shown in the flow (the style guide); live toasts come from toast.js.
type ToastData struct {
	Tone         Tone
	Text, Action string
}

// StatTileData is one dashboard tile, a link to the filtered list.
type StatTileData struct {
	Tone               Tone
	Label, Value, Hint string
	Href               string
	// HintAt, when set, puts a time into the hint: Hint, then the instant (HintAtText is its fallback
	// text, time.js shows it in the browser's zone like a day stamp), then HintAfter.
	HintAt     time.Time
	HintAtText string
	HintAfter  string
}

// KVRow is one label/value line of a card; Mono sets the value in the monospace font
// (addresses, MACs, versions, ids and commands). A name a person typed (a device name) is not one of them: IsName draws it
// in the page font at weight 500, like a device name in a table.
type KVRow struct {
	Key, Value string
	// Action is the one button that belongs to this row's value (Keep beside a device name that is on offer): it
	// follows the value inside the same cell.
	Action *Btn
	Mono   bool
	IsName bool
	Mask   bool      // a value the pixel comparison cannot reproduce (address, MAC, readings): data-visual-mask
	Wide   bool      // a long value: on a phone it takes the whole row of a compact card (Identity)
	Soft   bool      // a host:port, URL or file name: break opportunities at its natural points (Soft)
	At     time.Time // when set, Value is the fallback text of that instant and time.js shows it in the browser's zone
	Stamp  StampFmt  // how time.js shows At; the zero value is StampDateTime
	// Lead and Tail are the words around the instant ("Valid until " At ", by ann"); empty Value then takes the
	// instant's own fallback text.
	Lead, Tail string
}

// Tab is one tab of a tab row. Tabs are links (each tab its own route). A disabled tab says why.
type Tab struct {
	Label, Href string
	Current     bool
	Badge       int
	Reason      string // the tab cannot be used now: why (rendered disabled, reason as title)
}

// MenuItem is one entry of a row menu: a link (Href), or with Dialog a link that opens that
// dialog. Sep draws a rule before it; Why explains a disabled entry.
type MenuItem struct {
	Label, Href, Dialog string
	Danger, Sep         bool
	Disabled            bool
	Why                 string
}

// DialogData is a confirmation dialog: title, body, an optional list of what changes or a
// command input, Cancel and the confirming verb. The form posts to Action with the CSRF token.
type DialogData struct {
	ID, Title, Body string
	// BodyStamp, when its instant is set, is the body instead of Body: a sentence with a <time> in it (Stamped).
	BodyStamp    Stamped
	Changes      []KVRow // name and the command or value that changes
	Command      string  // non-empty: a Command input prefilled with this value
	Action, CSRF string
	Fields       []KVRow // hidden fields the form posts (Key = name)
	Verb         string
	// Alt, when set, is a second button that posts the same fields to AltAction instead
	// (Make compliant: "Choose Advisory rows…" opens the preview with the boxes).
	Alt, AltAction string
	Danger         bool
	Static         bool // drawn open in the flow (style guide), left alone by the scripts
	OOB            bool // sent as an htmx out-of-band swap of the dialog already on the page
}

// StepUpData is the "Confirm it's you" dialog of local account and SSO changes.
type StepUpData struct {
	ID, Name, Sub string // who confirms, and how they signed in
	What          string // the action that needs the confirmation
	IdP, IdPHref  string // identity provider and its re-authentication route (POST); empty for local accounts
	// IdPHref empty with an IdP: the re-authentication is the form's own submit (a POST to Action).
	Local        bool   // a local password is accepted (posted as passwordName() to Action)
	PasswordName string // the password input's name; empty: "current"
	Action, CSRF string
	Next         string  // local path the identity provider returns to (form field "next")
	Fields       []KVRow // further hidden fields the form posts (Key = name)
	Error        string
	Static       bool // drawn open in the flow (style guide), left alone by the scripts
	Open         bool // the page answers a refused action: dialog.js opens it as a modal at once
}

func (s StepUpData) passwordName() string {
	if s.PasswordName == "" {
		return "current"
	}
	return s.PasswordName
}

// ConfirmData is the page form of a confirmation, for browsers without JavaScript: the same
// look as the dialog, its own route.
type ConfirmData struct {
	Title, Heading, Text, Warning string
	// TextStamp, when its instant is set, is the text instead of Text: a sentence with a <time> in it (Stamped).
	TextStamp               Stamped
	Facts                   []KVRow
	Action, CSRF            string
	Fields                  []KVRow // hidden fields (Key = name)
	Ack                     string  // a required checkbox "ack" with this label; empty: none
	Verb                    string
	Danger                  bool
	CancelHref, CancelLabel string
	NoBusy                  bool // the answer is a file, not a page: form.js must not leave the button busy (#538)
	ACL                     bool // the broker ACL notice (BrokerACLNotice) stands above the facts (#840)
}

// showHeading says whether the card draws its own heading: only a heading that adds something to the page's title. A
// confirmation has one title, the dialog's (#1093); the card of a page whose heading equals it, or has none, draws none.
func (c ConfirmData) showHeading() bool { return c.Heading != "" && c.Heading != c.Title }

// FieldData is a label-above input with help text and the server's error for it.
type FieldData struct {
	Label, Name, Type, Value string
	Placeholder, Help, Error string
	Mono, Wide               bool
	Autocomplete             string
	NoAutocapitalize         bool // autocapitalize="off": a username or an address is not a sentence
	NoSpellcheck             bool // spellcheck="false": the same fields, and codes
	Autofocus                bool // the page's first focus: the field the server rejected, else the first input
}

// Option is one choice of a radio card group or segmented control.
type Option struct {
	Value, Label, Text string
	// TextStamp, when its instant is set, is the description instead of Text: a sentence with a <time> in it
	// (a release card's date). A control that cannot hold an element (a select's option) uses Text.
	TextStamp         Stamped
	Checked, Disabled bool
}

// AttProposal is a "Needs attention" proposal with two inline actions (POST forms). Version is
// the proposal content the admin saw; both forms send it, so an action never applies content that
// changed after the page was drawn.
type AttProposal struct {
	Name, Text                     string
	Primary, Secondary             string // button labels
	PrimaryAction, SecondaryAction string // POST paths
	Version                        string
}

// AttMore says what a capped list of "Needs attention" leaves out. With Href it is a link to the list that holds
// them all ("Show all 6"); without, plain text ("2 more") for a reason the device list cannot filter by. Label is the
// link's name for a screen reader, which tells the links apart ("Show all 6: Offline"); it starts with the visible
// text. The zero value shows nothing.
type AttMore struct {
	Text, Href, Label string
}

// AttGroup is one reason of "Needs attention": a heading and the entries of that reason, each device once.
type AttGroup struct {
	Title string
	Items []AttItem
	More  AttMore
}

// AttentionData is what AttentionList draws: a note on what the list holds, the proposals under their title (with More
// when there are more than shown), then one group per reason. Everything is text.
type AttentionData struct {
	Note       string
	PropsTitle string
	Props      []AttProposal
	PropsMore  AttMore
	Groups     []AttGroup
	CSRF       string
}

// AttItem is a "Needs attention" entry: the name and the reason, a link to it. A device whose connection is the
// reason carries Status, its status badge, and its colour is the badge's; an entry that is not a connection state (a
// weak signal, a policy deviation, the broker's ACL) has no Status and keeps the dot in Tone.
type AttItem struct {
	Tone             Tone
	Name, Text, Href string
	Status           *StatusBadgeData
}

// InfoTileData is one info tile (#1097): a label, a value, a hint, and now and then a bar or a button. It is no link,
// unlike StatTile, and it describes one thing (a device, a rule), where a stat tile counts the fleet. Key is the
// tile's name (data-tile), which the live refresh and the tests find it by. Word says the value is a word ("MQTT ·
// HTTP", "Ethernet"): 600 18px; a number ("-70 dBm", "12 / 14") is --fs-stat. A word that is a state takes Tone, the
// colour of its text variant. Note is a component beside the label ("Last reported"); the tile's children (a form
// with one small button) follow the hint. The hint is Hint, or HintPrefix and the instant HintAt (HintAtText is its
// fallback text, time.js shows it in the browser's zone like a day stamp). Everything else is text, never markup.
type InfoTileData struct {
	Key, Label, Value, Hint string
	Tone                    Tone
	Word                    bool
	Mask                    bool // a reading the pixel comparison cannot reproduce: data-visual-mask on the value and the bar
	Bar                     int  // a signal quality 0..100; 0 draws no bar
	BarTone                 Tone
	HintPrefix              string
	HintAt                  time.Time
	HintAtText              string
	Note                    templ.Component
}

func (t InfoTileData) valueClass() string {
	c := "itile-v"
	if t.Word {
		c += " itile-word"
	}
	if tc := t.Tone.classOrNone(); tc != "" {
		c += " " + tc
	}
	return c
}

// TxnData is a transaction card: id, title, state badge, the three-colour progress and a summary.
type TxnData struct {
	ID, Title, State, Summary, Href string
	StateTone                       Tone
	OK, Failed, Active, Total       int
	// Stopped devices were never started (a template transaction stops after the first failure); drawn grey.
	Stopped int
	// StartedText is "when it started" under the summary; empty draws none. When Started is set, StartedText is the
	// fallback text of that instant (StampRelative) and time.js shows it in the browser's zone.
	StartedText string
	Started     time.Time
}

// TxnRowData is one row of the transaction history.
type TxnRowData struct {
	ID, Title, Result, When, Href string
	Tone                          Tone
	WhenAt                        time.Time // when set, When is the fallback text of that instant (StampRelative) and time.js shows it in the browser's zone
}

// ConsoleLine is one line of the console: time, text, kind ("cmd", "err", "reply" for the
// device's answer to the command just sent, "alert" for its refusal or failure, or "" for other
// output). Datetime (RFC 3339) makes
// the time a <time> element; Who (who sent a command) follows the text, dimmed.
type ConsoleLine struct{ Time, Datetime, Text, Kind, Who string }

// ConsoleData is the device console: the lines, the prompt form and the quick commands (submit
// buttons below the panel that send their command). The forms post the field "command";
// Hidden fields go with every send (the console's "since", for example). ClearHref makes Clear
// a link (the page without the lines so far).
type ConsoleData struct {
	Title, Action, CSRF string
	Input               string // the prompt's value (a command sent back for correction)
	HistoryAdd          string // a command just sent that the browser may keep in its Arrow Up history ("" keeps nothing)
	ClearHref           string
	Hidden              []KVRow
	Lines               []ConsoleLine
	Quick               []QuickCmd
}

// QuickCmd is one quick command of the console: a submit button that sends Command, or, with Href, a link that opens
// the confirmation named by Dialog instead (a command that interrupts the device is never sent in one click, #1093).
// With Why the command cannot be sent now: the blocked button, named by the element WhyID.
type QuickCmd struct {
	Command, Href, Dialog string
	Why, WhyID            string
}

// DiffRow is one setting of a backup comparison; Changed marks a difference.
type DiffRow struct {
	Key, Old, New string
	Changed       bool
}

// DiffData is a comparison of two backups, side by side or unified.
type DiffData struct {
	OldLabel, NewLabel string
	// OldAt and NewAt, when set, are the instants the two sides were taken: each is then a <time> element (the label
	// is not used) that time.js shows in the browser's locale and zone.
	OldAt, NewAt time.Time
	Unified      bool
	Rows         []DiffRow
}

// NoteBlock is one block of release notes: "h" heading, "p" paragraph, "ul" list, "code" code
// block, "breaking" the breaking-change box (Text is its heading, Items its points). Text only:
// release notes never become markup (firmware/notes.go contract).
type NoteBlock struct {
	Kind, Text string
	Items      []string
}

// units is n of total as a share of 1000 units (the SVG bars' coordinate space).
func units(n, total int) int {
	if total <= 0 || n <= 0 {
		return 0
	}
	return n * 1000 / total
}

// barSeg is one segment of a bar: its x and width in the 1000-unit space and its tone.
type barSeg struct {
	X, W int
	Tone Tone
}

// segments lays out the parts of a bar left to right with gap units between non-empty parts.
func segments(gap int, parts ...struct {
	N    int
	Tone Tone
}) []barSeg {
	total := 0
	for _, p := range parts {
		total += p.N
	}
	var out []barSeg
	x := 0
	for _, p := range parts {
		w := units(p.N, total)
		if w == 0 {
			continue
		}
		out = append(out, barSeg{X: x, W: w, Tone: p.Tone})
		x += w
	}
	if len(out) > 1 { // shrink each part by its share of the gaps so the bar keeps its width
		g := gap * (len(out) - 1)
		x = 0
		for i := range out {
			out[i].W = out[i].W - g/len(out)
			if out[i].W < 1 {
				out[i].W = 1
			}
			out[i].X = x
			x += out[i].W + gap
		}
	}
	return out
}

// barGaps are the x positions, in the bar's 1000-unit space, of the dividers between neighbouring segments: the
// centre of each gap segments() leaves. The gap itself is 2 units, under a pixel on a narrow bar, so the divider is a
// line in the track's colour with a fixed stroke width (.bar-gap in app.css), not the gap's own width: it keeps the
// two tones apart by more than hue wherever the bar is drawn (#1268). One segment, or none, has no boundary.
func barGaps(segs []barSeg) []int {
	var out []int
	for i := 1; i < len(segs); i++ {
		out = append(out, (segs[i-1].X+segs[i-1].W+segs[i].X)/2)
	}
	return out
}

type part = struct {
	N    int
	Tone Tone
}

// healthSegments are the fleet health bar's parts: online, offline, unknown.
func healthSegments(ok, off, unk int) []barSeg {
	return segments(2, part{ok, ToneOK}, part{off, ToneDanger}, part{unk, ToneNeutral})
}

// complianceSegments are a rule's bar: match, deviate, not judged. Same math as the health bar.
func complianceSegments(match, deviate, unknown int) []barSeg {
	return segments(2, part{match, ToneOK}, part{deviate, ToneWarn}, part{unknown, ToneNeutral})
}

// txnSegments are the transaction bar's parts: succeeded, failed, running. The devices still
// waiting (Total minus the three) are the empty rest of the bar, as in the prototype.
func txnSegments(t TxnData) []barSeg {
	rest := max(t.Total-t.OK-t.Failed-t.Active-t.Stopped, 0)
	segs := segments(1, part{t.OK, ToneOK}, part{t.Failed, ToneDanger}, part{t.Active, ToneAccent}, part{t.Stopped, ToneNeutral}, part{rest, toneRest})
	out := segs[:0]
	for _, s := range segs {
		if s.Tone != toneRest {
			out = append(out, s)
		}
	}
	return out
}

// segCell is the pitch of a pattern in the SegBar's 1000-unit width: a stripe and a gap are each this wide. Drawn
// at its narrowest (the dashboard card on a 375 px phone, about 300 px wide) a cell is still 3 px, so an
// antialiased edge never blends a stripe into the track.
const segCell = 10

// segRect is one rectangle of a SegBar part, in the bar's 1000 x 6 space.
type segRect struct{ X, Y, W, H int }

// segRects draws a part. Colour alone told done, running and failed apart (about 1.1:1 between done and failed in
// dark), so each state has a shape: a succeeded or other part is one solid bar, a running part is vertical stripes
// with gaps, a failed part is a checker over two rows. Every rect stays inside the part; the sliver a part's end
// would leave (under 3 units, under 1 px) is not drawn.
func segRects(s barSeg) []segRect {
	if s.Tone != ToneAccent && s.Tone != ToneDanger {
		return []segRect{{X: s.X, Y: 0, W: s.W, H: 6}}
	}
	step, h := 2*segCell, 6 // running: a stripe, then a gap
	if s.Tone == ToneDanger {
		step, h = segCell, 3 // failed: cells side by side, alternately in the top and the bottom row
	}
	var out []segRect
	for i, x := 0, s.X; x < s.X+s.W; i, x = i+1, x+step {
		w := min(segCell, s.X+s.W-x)
		if len(out) > 0 && w < 3 {
			break
		}
		y := 0
		if s.Tone == ToneDanger {
			y = 3 * (i % 2)
		}
		out = append(out, segRect{X: x, Y: y, W: w, H: h})
	}
	return out
}

// OffPolicy is the visible words of a device's policy deviations: "1 off policy", "3 off policy". Short on purpose:
// "Online" and this badge share the 170 px Status column of the list on one line (a longer text wraps the badge under
// Online, and the bubble of Online then lies over the row above).
func OffPolicy(n int) string { return itoa(n) + " off policy" }

// toneRest marks the undrawn rest of a bar.
const toneRest Tone = "rest"

// AvatarTone picks the avatar's colour from the username, so a user keeps one colour everywhere.
func AvatarTone(username string) Tone {
	tones := [...]Tone{ToneAccent, ToneOK, ToneWarn, ToneDanger, ToneNeutral}
	h := fnv.New32a()
	_, _ = h.Write([]byte(username)) // a hash.Hash never returns an error from Write
	return tones[h.Sum32()%uint32(len(tones))]
}

func itoa(n int) string { return strconv.Itoa(n) }

// stampOf is the format of a KVRow's time: StampDateTime unless the row names another.
func (r KVRow) stampOf() StampFmt {
	if r.Stamp.Key == "" {
		return StampDateTime
	}
	return r.Stamp
}

// CardData is a card that needs more than a title and a line: its place in the phone stack, a class of its own, an id,
// another element, the heading a label names, or attributes of a live swap. Every field is text, a number or a flag,
// never markup; the card's body is the children.
type CardData struct {
	Title, Sub string
	// ID is the card's id (a link target, a swap target). Class is the page's own class ("fw-sel"); Rank is the phone
	// stack place (StackRank), 0 for none. The classes come out as rank, ucard, then Class, always in that order.
	ID, Class string
	Rank      int
	// Tag is the element: "section" (also for "" and anything unknown), "article", "aside", "form" or "div". A fixed
	// set, never free text. A card that is an element of a live swap keeps its element (div), a card that is a
	// complementary column is an aside, a card that is a form is a form (its method and action go in Attrs).
	Tag string
	// LabelledBy is the id of the card's heading: with a Title the h2 gets it, without one the heading is in the
	// children (a heading with markup, several headings) and the id names that. Aria is an aria-label for a card
	// that has no visible heading.
	LabelledBy, Aria string
	Attrs            templ.Attributes // see pageAttrs
}

func (c CardData) classes() string {
	var parts []string
	if r := StackRank(c.Rank); r != "" {
		parts = append(parts, r)
	}
	parts = append(parts, "ucard")
	if c.Class != "" {
		parts = append(parts, c.Class)
	}
	return strings.Join(parts, " ")
}

func (c CardData) tag() string {
	switch c.Tag {
	case "article", "aside", "form", "div":
		return c.Tag
	}
	return "section"
}

func (c CardData) attrs() templ.Attributes { return pageAttrs(c.Attrs) }
