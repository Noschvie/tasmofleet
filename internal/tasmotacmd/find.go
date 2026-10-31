package tasmotacmd

import (
	"strconv"
	"strings"
)

// Found is a typed command resolved in one catalogue.
type Found struct {
	// Name is the command as the table spells it ("SetOption", "FriendlyName").
	Name string
	// Entries are the table entries of that name, one per driver that defines it.
	Entries []*Command
	// Index is the number typed after the name; HasIndex is false when none was typed.
	Index    int
	HasIndex bool
}

// Find resolves a command as it is typed ("SetOption19", "power1", "OtaUrl"; arguments after a
// space are ignored). A name is looked up whole first, then without trailing digits, which are
// the index. ok is false for a name the catalogue does not have.
func (c *Catalogue) Find(cmd string) (Found, bool) {
	c.once.Do(c.buildIndex)
	f := strings.Fields(cmd)
	if len(f) == 0 {
		return Found{}, false
	}
	name := f[0]
	if es := c.byName[strings.ToLower(name)]; len(es) > 0 {
		return Found{Name: es[0].Command, Entries: es}, true
	}
	end := len(name)
	for end > 0 && name[end-1] >= '0' && name[end-1] <= '9' {
		end--
	}
	if end == 0 || end == len(name) {
		return Found{}, false
	}
	n, err := strconv.Atoi(name[end:])
	if err != nil {
		return Found{}, false
	}
	es := c.byName[strings.ToLower(name[:end])]
	if len(es) == 0 {
		return Found{}, false
	}
	return Found{Name: es[0].Command, Entries: es, Index: n, HasIndex: true}, true
}

// Range is the index range a command accepts, when the source states one as numbers.
func (i *Index) Range() (lo, hi int, ok bool) {
	if i == nil || i.Max == "" {
		return 0, 0, false
	}
	hi, err := strconv.Atoi(i.Max)
	if err != nil {
		return 0, 0, false
	}
	if i.Min != "" {
		if lo, err = strconv.Atoi(i.Min); err != nil {
			return 0, 0, false
		}
	}
	return lo, hi, hi >= lo
}

// IndexSpan reports whether the command is worth an index field: the source states a range with
// more than one index (AdcGpio, which takes only 1, is not).
func (c *Command) IndexSpan() (lo, hi int, ok bool) {
	lo, hi, ok = c.Index.Range()
	return lo, hi, ok && hi >= 2
}

// IndexOK reports whether the typed index is acceptable to at least one entry; an entry
// without a stated range accepts any index.
func (f Found) IndexOK() bool {
	if !f.HasIndex {
		return true
	}
	for _, e := range f.Entries {
		lo, hi, ok := e.Index.Range()
		if !ok || (f.Index >= lo && f.Index <= hi) {
			return true
		}
	}
	return false
}

// IndexRange is the range of the first entry that states one.
func (f Found) IndexRange() (lo, hi int, ok bool) {
	for _, e := range f.Entries {
		if lo, hi, ok := e.IndexSpan(); ok {
			return lo, hi, true
		}
	}
	return 0, 0, false
}

// Env is what is known about a device's build.
type Env struct {
	// Chip is the chip family: "esp8266", "esp32", "esp32s3", ... ; empty when unknown.
	Chip string
	// Variant is the firmware variant as TasmoFleet names it ("tasmota", "lite", "minimal", ...);
	// empty or "unknown" for a build we cannot place (a custom build).
	Variant string
}

// variantMacro maps a firmware variant to the FIRMWARE_ macro its build defines; the plain
// ESP8266 build defines none. These are the build names of Tasmota's own platformio
// environments, not facts about commands.
var variantMacro = map[string]string{
	"minimal": "FIRMWARE_MINIMAL", "lite": "FIRMWARE_LITE", "sensors": "FIRMWARE_SENSORS",
	"knx": "FIRMWARE_KNX_NO_EMULATION", "display": "FIRMWARE_DISPLAYS", "ir": "FIRMWARE_IR",
	"zbbridge": "FIRMWARE_ZBBRIDGE", "webcam": "FIRMWARE_WEBCAM", "bluetooth": "FIRMWARE_BLUETOOTH",
	"lvgl": "FIRMWARE_LVGL", "nspanel": "FIRMWARE_NSPANEL", "zbbrdgpro": "FIRMWARE_ZBBRDGPRO",
	"safeboot": "FIRMWARE_SAFEBOOT",
}

// build returns the Builds key of an environment and the FIRMWARE_ macro it defines ("" for none).
func (e Env) build() (key, macro string, ok bool) {
	var fam string
	switch {
	case e.Chip == "esp8266":
		fam = "esp8266"
	case strings.HasPrefix(e.Chip, "esp32"):
		fam = "esp32"
	default:
		return "", "", false
	}
	switch v := e.Variant; v {
	case "", "unknown":
		return "", "", false
	case "tasmota", "4M":
		if fam == "esp32" {
			return fam + "/FIRMWARE_TASMOTA32", "FIRMWARE_TASMOTA32", true
		}
		return fam + "/tasmota", "", true
	default:
		m, known := variantMacro[v]
		if !known {
			return "", "", false
		}
		return fam + "/" + m, m, true
	}
}

// isDefined answers a build condition for a device: what the catalogue's default configuration of its
// variant says. A name it has no fact about is Unknown, never "not defined".
func (c *Catalogue) isDefined(e Env) func(string) Tri {
	key, macro, built := e.build()
	return func(name string) Tri {
		switch {
		case name == "ESP8266" || name == "ESP32":
			if e.Chip == "" {
				return Unknown
			}
			return triOf(name == "ESP8266" && e.Chip == "esp8266" || name == "ESP32" && strings.HasPrefix(e.Chip, "esp32"))
		case strings.HasPrefix(name, "CONFIG_IDF_TARGET_ESP32"):
			if e.Chip == "" {
				return Unknown
			}
			return triOf(strings.ToLower(strings.TrimPrefix(name, "CONFIG_IDF_TARGET_")) == e.Chip)
		case strings.HasPrefix(name, "FIRMWARE_"):
			if !built {
				return Unknown
			}
			return triOf(name == macro)
		case strings.HasPrefix(name, "USE_"):
			list, have := c.Builds[key]
			if !built || !have {
				return Unknown
			}
			for _, n := range list {
				if n == name {
					return True
				}
			}
			return False
		}
		return Unknown
	}
}

func triOf(b bool) Tri {
	if b {
		return True
	}
	return False
}

// Lacks lists what a device's build certainly misses for the command: the conditions of its table
// entry that are false for the device's variant, as "USE_SUNRISE" or, when no define is named,
// the condition itself ("ESP32"). Empty when the command is there or when nothing is known. With
// several entries of one name (several drivers) it is empty as soon as one of them can be there.
func (c *Catalogue) Lacks(f Found, e Env) []string {
	def := c.isDefined(e)
	var first []string
	for i, en := range f.Entries {
		var miss []string
		for _, req := range en.Requires {
			if EvalCond(req, def) != False {
				continue
			}
			named := false
			for _, n := range CondNames(req) {
				if strings.HasPrefix(n, "USE_") && def(n) == False {
					miss = append(miss, n)
					named = true
				}
			}
			if !named {
				miss = append(miss, condText(req))
			}
		}
		if len(miss) == 0 {
			return nil
		}
		if i == 0 {
			first = miss
		}
	}
	return dedupeStrings(first)
}

func condText(req string) string {
	r := strings.Join(strings.Fields(req), " ")
	if rest, ok := strings.CutPrefix(r, "!"); ok && !strings.ContainsAny(rest, " (&|") {
		return "not " + rest
	}
	return r
}
