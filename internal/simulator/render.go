package simulator

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// view is the live state a reply is rendered with.
type view struct {
	now       time.Time
	startup   time.Time
	uptimeSec int
	relays    []bool
	light     bool // PWM light: Status 11 carries Dimmer and CT
	noRelay   bool // a sensor-only device: no POWER key anywhere
	otaURL    string
	bootCount int
	mqttCount int
	reason    string
	image     string
	version   string
	kv        map[string]string
}

var (
	reTime      = regexp.MustCompile(`"Time":"[^"]*"`)
	reUTC       = regexp.MustCompile(`"UTC":"[^"]*"`)
	reLocal     = regexp.MustCompile(`"Local":"[^"]*"`)
	reStartup   = regexp.MustCompile(`"StartupUTC":"[^"]*"`)
	reUptime    = regexp.MustCompile(`"Uptime":"[^"]*"`)
	reUptimeSec = regexp.MustCompile(`"UptimeSec":\d+`)
	reHeap      = regexp.MustCompile(`"Heap":(\d+)`)
	reBoot      = regexp.MustCompile(`"BootCount":\d+`)
	reReason    = regexp.MustCompile(`"RestartReason":"[^"]*"`)
	reMqttCount = regexp.MustCompile(`"MqttCount":\d+`)
	rePowerName = regexp.MustCompile(`"POWER":"(ON|OFF)"`)
	rePowerMask = regexp.MustCompile(`"Power":"[01]"`)
	reLightKeys = regexp.MustCompile(`,"(Dimmer|Color|HSBColor|White|CT|Channel|Scheme|Width|Fade|Speed|LedTable)":("[^"]*"|\[[^\]]*\]|-?[0-9]+)`)
	reOtaURL    = regexp.MustCompile(`"OtaUrl":"[^"]*"`)
)

func formatUptime(sec int) string {
	return fmt.Sprintf("%dT%02d:%02d:%02d", sec/86400, sec%86400/3600, sec%3600/60, sec%60)
}

// render returns a captured file with the device's identity substituted and,
// unless verbatim, the live fields recomputed. Everything else stays the
// recorded text, so decimals and key order are exactly as the device sent.
func (d *simDevice) render(file string, v view) []byte {
	raw, ok := d.cap.files[file]
	if !ok {
		return nil
	}
	s := string(raw)
	var pairs []string
	if v.image != d.cap.image {
		pairs = append(pairs, "(release-"+d.cap.image+")", "(release-"+v.image+")")
	}
	if v.version != d.cap.version {
		pairs = append(pairs, d.cap.version, v.version)
	}
	if d.topic != d.cap.baseTopic {
		pairs = append(pairs, d.cap.baseTopic, d.topic)
	}
	if !d.f.x.Verbatim {
		pairs = append(pairs, d.cap.ip, d.host, d.cap.mac, d.mac,
			strings.ReplaceAll(d.cap.mac, ":", ""), strings.ReplaceAll(d.mac, ":", ""))
	}
	if len(pairs) > 0 {
		s = strings.NewReplacer(pairs...).Replace(s)
	}
	s = d.applySettings(s, v.kv)
	if file == "status-5.json" && d.spec.Wired {
		s = d.wiredStatus5(s)
	}
	if d.f.x.Verbatim {
		return []byte(s)
	}
	const layout = "2006-01-02T15:04:05"
	rep := func(re *regexp.Regexp, with string) { s = re.ReplaceAllLiteralString(s, with) }
	rep(reTime, `"Time":"`+v.now.Format(layout)+`"`)
	rep(reUTC, `"UTC":"`+v.now.UTC().Format(layout)+`Z"`)
	rep(reLocal, `"Local":"`+v.now.Format(layout)+`"`)
	rep(reStartup, `"StartupUTC":"`+v.startup.UTC().Format(layout)+`"`)
	rep(reUptime, `"Uptime":"`+formatUptime(v.uptimeSec)+`"`)
	rep(reUptimeSec, `"UptimeSec":`+strconv.Itoa(v.uptimeSec))
	jitter := v.uptimeSec/10%3 - 1
	s = reHeap.ReplaceAllStringFunc(s, func(m string) string {
		n, err := strconv.Atoi(reHeap.FindStringSubmatch(m)[1])
		if err != nil {
			return m
		}
		return `"Heap":` + strconv.Itoa(n+jitter)
	})
	rep(reBoot, `"BootCount":`+strconv.Itoa(v.bootCount))
	rep(reReason, `"RestartReason":`+jsonString(v.reason))
	rep(reMqttCount, `"MqttCount":`+strconv.Itoa(v.mqttCount))
	// The captures are mostly lights; a device that is not one reports no
	// light state, which is how the OTA engine tells a light from a relay.
	if !v.light {
		s = reLightKeys.ReplaceAllString(s, "")
	}
	rep(rePowerName, powerFields(v.relays, v.light && !strings.Contains(s, `"Dimmer"`)))
	if v.noRelay {
		// the capture's POWER key goes, and with it the comma that held it in place
		s = strings.NewReplacer(`"POWER":"ON",`, "", `"POWER":"OFF",`, "", `,"POWER":"ON"`, "", `,"POWER":"OFF"`, "", `"POWER":"ON"`, "", `"POWER":"OFF"`, "").Replace(s)
	}
	rep(rePowerMask, `"Power":"`+strconv.FormatUint(powerMask(v.relays), 16)+`"`)
	rep(reOtaURL, `"OtaUrl":`+jsonString(v.otaURL))
	return []byte(s)
}

func onOff(b bool) string {
	if b {
		return "ON"
	}
	return "OFF"
}

// powerFields renders the POWER keys like Tasmota: POWER for one relay,
// POWER1..n for several.
func powerFields(relays []bool, light bool) string {
	extra := ""
	if light {
		extra = `,"Dimmer":100,"CT":250`
	}
	if len(relays) <= 1 {
		return `"POWER":"` + onOff(len(relays) == 1 && relays[0]) + `"` + extra
	}
	parts := make([]string, len(relays))
	for i, r := range relays {
		parts[i] = `"POWER` + strconv.Itoa(i+1) + `":"` + onOff(r) + `"`
	}
	return strings.Join(parts, ",") + extra
}

func powerMask(relays []bool) uint64 {
	var m uint64
	for i, r := range relays {
		if r {
			m |= 1 << i
		}
	}
	return m
}

var (
	reStatusNET   = regexp.MustCompile(`"StatusNET":\{[^}]*`)
	reIdleEthObj  = regexp.MustCompile(`,"Ethernet":\{[^}]*\}`)
	ethKeys       = []string{"Hostname", "IPAddress", "Gateway", "Subnetmask", "DNSServer1", "DNSServer2", "Mac"}
	idleEthValues = []string{"IPAddress", "Gateway", "Subnetmask", "DNSServer1"}
)

// wiredStatus5 turns the captured Status 5 into an Ethernet board's: the Wi-Fi values at the top
// level stay unset and the "Ethernet" object carries the active interface (own MAC, as on a
// board with both). A capture of a board that has Ethernet support already holds an idle
// "Ethernet" object; it is dropped first, or the result would hold two.
func (d *simDevice) wiredStatus5(s string) string {
	s = reIdleEthObj.ReplaceAllString(s, "")
	m := reStatusNET.FindString(s)
	if m == "" {
		return s
	}
	value := func(k string) string {
		if f := regexp.MustCompile(`"` + k + `":"([^"]*)"`).FindStringSubmatch(m); f != nil {
			return f[1]
		}
		return ""
	}
	parts := make([]string, 0, len(ethKeys))
	for _, k := range ethKeys {
		v := value(k)
		if k == "Mac" {
			v = d.ethMAC()
		}
		parts = append(parts, `"`+k+`":"`+v+`"`)
	}
	idle := m
	for _, k := range idleEthValues {
		idle = regexp.MustCompile(`"`+k+`":"[^"]*"`).ReplaceAllString(idle, `"`+k+`":"0.0.0.0"`)
	}
	return strings.Replace(s, m, idle+`,"Ethernet":{`+strings.Join(parts, ",")+`}`, 1)
}

// ethMAC is the Ethernet MAC of a wired device, different from its Wi-Fi MAC.
func (d *simDevice) ethMAC() string {
	sum := sha256.Sum256([]byte(d.spec.Name))
	return fmt.Sprintf("02:00:5E:%02X:%02X:%02X", sum[3], sum[4], sum[5])
}
