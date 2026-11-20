package transport

import (
	"fmt"
	"strings"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// fwReachTTL is how long a "no" stays true (D11): the network may have changed since.
const fwReachTTL = 24 * time.Hour

// defaultFullTopic is what a device with an empty FullTopic uses.
const defaultFullTopic = "%prefix%/%topic%/"

// Paths is the answer of Choose: one path per action and the sentence for each.
type Paths struct {
	// Commands are reads (Status N, identity refresh): they may go over MQTT with an unknown own
	// GroupTopic. Writes are commands that change the device (settings, Make compliant, non-read
	// console commands, backlog): they follow the strict rule of the transfers.
	Commands, Writes, Backup, Restore, Update Path
	// Reasons has one entry per action, in the order Commands, Writes, Backup, Restore, Update, each with a
	// non-empty Text.
	Reasons []Reason
}

// Reason returns the sentence for an action.
func (p Paths) Reason(a Action) string {
	for _, r := range p.Reasons {
		if r.Action == a {
			return r.Text
		}
	}
	return ""
}

// Choose decides, from facts alone, how each action reaches the device. It is pure: no clock, no
// I/O (Facts.Now is the clock). The order is fixed (spec §5.x):
//
//   - Commands (reads): MQTT when usable (#456), unless MQTT commands went unanswered or the device's MQTT
//     connection was just changed while HTTP works; else HTTP unless HTTP is failing.
//   - Writes: MQTT when usable under the strict rule (#605), with the same two exceptions; else HTTP unless
//     HTTP is failing.
//   - Backup: MQTT FileDownload first when usable for a transfer (strict rule, password set and not refused; #607),
//     with the same two exceptions as writes; else HTTP (/dl) unless HTTP is failing.
//   - Restore: MQTT FileUpload Type 2 first under the backup's rule (#607); else HTTP (the web UI's restore upload)
//     unless HTTP is failing.
//   - Update, pull: the device downloads the image from TasmoFleet's firmware server over HTTP; OtaUrl and Upgrade
//     go over MQTT first (DownloadMQTT) under the rule of writes, with the same two exceptions (#608), else over
//     HTTP (DownloadHTTP) unless HTTP is failing.
//   - Update, push where possible: today's HTTP upload decision while HTTP works (a push that fell back to the
//     download is a pull); with HTTP failing the upload over MQTT (only with the experimental checkbox, a fitting
//     image and a usable password), else the download over MQTT.
//
// Reads go MQTT first since #456, writes since #605, backups and restores since #607, the commands of an update since
// #608. The upload of a push stays on HTTP while HTTP works (the coordinator's decision of 2026-11-17).
func Choose(f Facts) Paths {
	var p Paths
	var t string
	p.Commands, t = chooseCommands(f)
	p.Reasons = append(p.Reasons, Reason{ActCommands, p.Commands, t})
	p.Writes, t = chooseWrites(f)
	p.Reasons = append(p.Reasons, Reason{ActWrites, p.Writes, t})
	p.Backup, t = chooseBackup(f)
	p.Reasons = append(p.Reasons, Reason{ActBackup, p.Backup, t})
	p.Restore, t = chooseRestore(f)
	p.Reasons = append(p.Reasons, Reason{ActRestore, p.Restore, t})
	p.Update, t = chooseUpdate(f)
	p.Reasons = append(p.Reasons, Reason{ActUpdate, p.Update, t})
	if httpFailing(f) && f.HTTP.Kind == HTTPFailAPIRefused {
		// An action that has no way left because the device's HTTP API is off says how to get it back.
		for i, r := range p.Reasons {
			if r.Path == None {
				p.Reasons[i].Text += ". " + tasmota.APIOffFix
			}
		}
	}
	return p
}

func httpFailing(f Facts) bool { return f.HTTP.State == HTTPFailing }

// fwReach is the firmware-server reach with the 24 h expiry of "no" applied.
func fwReach(f Facts) model.FWReachState {
	// a "no" without a time cannot be dated, so it cannot be trusted either
	if f.FWReach.State == model.FWReachNo && (f.FWReach.At.IsZero() || f.Now.Sub(f.FWReach.At) > fwReachTTL) {
		return model.FWReachUnknown
	}
	return f.FWReach.State
}

func fwReachNo(f Facts) bool { return fwReach(f) == model.FWReachNo }

// fwBlocker says why the device cannot fetch from the firmware server, "" when it may.
func fwBlocker(f Facts) string {
	switch {
	case !f.FirmwareBaseURL:
		return "FIRMWARE_BASE_URL is not set, so the device has no firmware server address"
	case fwReachNo(f):
		s := "the device does not reach TasmoFleet's firmware server"
		if !f.FWReach.At.IsZero() {
			s += " (since " + f.FWReach.At.UTC().Format("2006-01-02 15:04 UTC") + ")"
		}
		return s
	}
	return ""
}

// fullTopic normalises a FullTopic for comparison: empty is Tasmota's default, a trailing '/' is
// not significant.
func fullTopic(s string) string {
	if s == "" {
		s = defaultFullTopic
	}
	return strings.TrimRight(s, "/")
}

// SameFullTopic reports whether two FullTopics are the same pattern (fullTopic): the one rule for "the device's
// FullTopic is TasmoFleet's MQTT_FULLTOPIC", which the facts source uses too (#812).
func SameFullTopic(a, b string) bool { return fullTopic(a) == fullTopic(b) }

// groupTopicKnown reports whether the stored GroupTopic can be trusted: set, stamped, and not
// older than the last Status 1 read (that read would have replaced it; an INFO1 or Status 1 that
// carried none leaves the old value behind, review #264 L2).
func groupTopicKnown(m MQTTFacts) bool {
	return m.GroupTopic != "" && !m.GroupTopicReadAt.IsZero() && !m.GroupTopicReadAt.Before(m.LastProbeAt)
}

// mqttBlocker says why the device is not usable over MQTT, "" when it is. strict adds the
// requirement that the GroupTopic is known: anything but a plain command must not go to a topic
// that might be shared, a transfer or an upgrade there would reach the wrong devices.
func mqttBlocker(f Facts, strict bool) string {
	m := f.MQTT
	switch {
	case !m.BusConnected:
		return "TasmoFleet is not connected to its MQTT broker"
	case !m.Ours:
		return "the device does not use TasmoFleet's MQTT broker"
	case m.Topic == "":
		return "the device has no MQTT topic"
	case !TopicShapeOK(m.Topic):
		return "the device's MQTT topic contains characters that are not allowed"
	case m.Topic == DefaultGroupTopic:
		return "the device's topic is the default group topic, which many devices listen on"
	case !m.TopicExclusive:
		return "another device listens on this topic (its Topic or GroupTopic), or one of the device's own group topics is this topic"
	case strict && !m.TopicTakenSince.IsZero(): // #852: a write or a transfer there would reach that device too
		return topicTakenText(m)
	case !SameFullTopic(m.FullTopic, m.OurFullTopic):
		return "the device's FullTopic differs from TasmoFleet's MQTT_FULLTOPIC"
	case m.GroupTopic != "" && m.GroupTopic == m.Topic: // even a stale value: it only ever adds a refusal
		return "the device's topic is also its GroupTopic, so a message would reach other devices"
	case strict && len(m.OthersGroupUnread) > 0:
		return othersUnreadText(m.OthersGroupUnread)
	case strict && !groupTopicKnown(m):
		return "the device's GroupTopic has not been read recently, so TasmoFleet cannot rule out that its topic is shared; refresh the device's identity to read it"
	case !m.LWTOnline:
		return "the device is not online on MQTT"
	}
	return ""
}

// passwordBlocker says why a transfer over MQTT cannot carry the device's MqttPassword. Which modes give a
// password is model.FilePassword's rule alone: the facts source turns a mode that gives none into Unusable.
func passwordBlocker(f Facts, what string) string {
	if !f.Password.BadAt.IsZero() && f.Password.Unusable == "" {
		return "the device refused its MQTT password (since " + f.Password.BadAt.UTC().Format("2006-01-02 15:04 UTC") + "); enter the right one to " + what + " over MQTT"
	}
	return passwordMissing(f, what)
}

// passwordMissing says why the device has no MqttPassword to send, the refusal mark aside.
func passwordMissing(f Facts, what string) string {
	switch {
	case f.Password.Unusable != "":
		return f.Password.Unusable
	case f.Password.Mode == model.MQTTPasswordNone:
		return "enter the device's MQTT password to " + what + " over MQTT"
	case !knownMode(f.Password.Mode):
		return "the device's MQTT password mode is not recognised"
	}
	return ""
}

// knownMode reports whether model.FilePassword gives a password for mode at all. It is asked with stand-in
// values, so the list of modes stays in that one function.
func knownMode(mode model.MQTTPasswordMode) bool {
	_, ok := model.FilePassword(model.Device{MQTTPasswordMode: mode, MQTTPassword: "-"}, "-")
	return ok
}

// PasswordTestBlocker says why the password test (#609) cannot run for the device, "" when it can: it follows the
// strict rule of transfers, since it starts a FileDownload, but ignores the refusal mark: checking a refused
// password again is what the test is for. The router and the device page's Test button ask this one function.
func PasswordTestBlocker(f Facts) string {
	if s := mqttBlocker(f, true); s != "" {
		return s
	}
	return passwordMissing(f, "test it")
}

// transferBlocker combines the MQTT and password requirements of a transfer.
func transferBlocker(f Facts, what string) string {
	if s := mqttBlocker(f, true); s != "" {
		return s
	}
	return passwordBlocker(f, what)
}

// BackupPasswordBlocker says why a backup cannot go over MQTT when the device is usable there for a transfer in every
// other way and only a usable MQTT password is missing (not set, refused, or none to send; #997): "" when MQTT is not
// usable for another reason, or when the password is usable. The router asks it before a backup over HTTP (a device
// that does not answer over HTTP then needs the password), and the pages ask it before they link the password form,
// so the rule exists once.
func BackupPasswordBlocker(f Facts) string {
	if mqttBlocker(f, true) != "" {
		return ""
	}
	return passwordBlocker(f, "back up")
}

// noPath builds the sentence for None: what is wrong with each way.
func noPath(parts ...string) string {
	var out []string
	for _, s := range parts {
		if s != "" {
			out = append(out, s)
		}
	}
	return "no path: " + strings.Join(out, ", and ")
}

// chooseCommands: reads go over MQTT first (#456) whenever the device is on TasmoFleet's broker and
// live there, whatever HTTP's state is, unless another device answers on its topic (#852), MQTT commands of the
// device just went unanswered or its MQTT connection was just changed, and HTTP works. Only a device MQTT cannot
// serve is read over HTTP, and when HTTP is failing too there is no path. Writes and transfers never go to a topic
// another device answers on (mqttBlocker, strict).
func chooseCommands(f Facts) (Path, string) {
	why := mqttBlocker(f, false)
	switch {
	case why == "" && httpFailing(f):
		return MQTT, "over MQTT, because " + httpFailText(f.HTTP)
	case why == "" && !f.MQTT.TopicTakenSince.IsZero():
		return HTTP, "over the device's HTTP API, because " + topicTakenText(f.MQTT)
	case why == "" && !f.MQTT.ConnChangedSince.IsZero():
		return HTTP, "over the device's HTTP API, because " + connChangedText(f.MQTT)
	case why == "" && !f.MQTT.ReadsUnansweredSince.IsZero():
		return HTTP, "over the device's HTTP API, because a command over MQTT got no answer from the device (since " +
			f.MQTT.ReadsUnansweredSince.UTC().Format("15:04 UTC") + "); MQTT is tried again after a few minutes"
	case why == "":
		return MQTT, "over MQTT first (the device is online on TasmoFleet's broker); over HTTP once if the device does not answer or nothing could be published"
	case !httpFailing(f):
		return HTTP, "over the device's HTTP API, because MQTT is not usable: " + why
	}
	return None, noPath(httpFailText(f.HTTP), "MQTT is not usable: "+why)
}

// chooseWrites: changes go over MQTT first (#605) when the device is usable there under the strict rule (its own
// GroupTopic read, no other device's unread), unless MQTT commands of the device just went unanswered or its MQTT
// connection was just changed, and HTTP works.
// Otherwise HTTP, and when HTTP is failing too there is no path. What a change carries (a password, the MQTT
// connection, a GPIO template) is no reason for HTTP; the router sends a change over HTTP only when MQTT published
// nothing, as for a command the channel refuses because it runs other commands. The sentence says so, since it
// stands for every change.
func chooseWrites(f Facts) (Path, string) {
	why := mqttBlocker(f, true)
	switch {
	case why == "" && httpFailing(f):
		return MQTT, "over MQTT, because " + httpFailText(f.HTTP)
	case why == "" && !f.MQTT.ConnChangedSince.IsZero():
		return HTTP, "over the device's HTTP API, the switches too, because " + connChangedText(f.MQTT)
	case why == "" && !f.MQTT.ReadsUnansweredSince.IsZero():
		return HTTP, "over the device's HTTP API, the switches too, because a command over MQTT got no answer from the device (since " +
			f.MQTT.ReadsUnansweredSince.UTC().Format("15:04 UTC") + "); MQTT is tried again after a few minutes"
	case why == "":
		return MQTT, "over MQTT first (the device is online on TasmoFleet's broker and its topic is its own); over HTTP if nothing could be published, " +
			"as for a command that runs other commands (Json, If, WebSend, a rule text), which MQTT does not carry"
	case !httpFailing(f):
		return HTTP, "over the device's HTTP API, the switches too, because changing the device over MQTT is not possible: " + why
	}
	return None, noPath(httpFailText(f.HTTP), "changing the device over MQTT is not possible: "+why)
}

// chooseBackup: a backup goes over MQTT first (#607, the product owner's rule of 2026-11-14) whenever the device is
// usable there for a transfer (transferBlocker: the strict rule of writes and a usable MQTT password), with the two
// exceptions of writes: MQTT commands of the device just went unanswered, or its MQTT connection was just changed,
// and HTTP works. Otherwise HTTP (/dl), and when HTTP is failing too there is no path. The files archive follows the
// dump over MQTT only where the firmware sends files (FileDownload Type 8, #924; the backup decides, not this rule),
// else it needs HTTP; the router sends the dump over HTTP only when MQTT published nothing.
func chooseBackup(f Facts) (Path, string) {
	why := transferBlocker(f, "back up")
	switch {
	case why == "" && httpFailing(f):
		return MQTT, "over MQTT (FileDownload), because " + httpFailText(f.HTTP)
	case why == "" && !f.MQTT.ConnChangedSince.IsZero():
		return HTTP, "over the device's HTTP API (/dl), because " + connChangedText(f.MQTT)
	case why == "" && !f.MQTT.ReadsUnansweredSince.IsZero():
		return HTTP, "over the device's HTTP API (/dl), because " + unansweredText(f.MQTT)
	case why == "":
		return MQTT, "over MQTT first (FileDownload; the device is online on TasmoFleet's broker and its topic is its own), " +
			"the files archive over MQTT too where the firmware sends files (FileDownload Type 8, USE_UFILESYS_MQTT), " +
			"else over HTTP when HTTP answers; over HTTP if nothing could be published"
	case !httpFailing(f):
		return HTTP, "over the device's HTTP API (/dl), because a backup over MQTT is not possible: " + why
	}
	return None, noPath(httpFailText(f.HTTP), "a backup over MQTT is not possible: "+why)
}

// chooseRestore is the backup's rule (#607): the dump goes over MQTT first (FileUpload Type 2) when the device is
// usable there for a transfer, else with the web UI's restore upload over HTTP (#430: the firmware server plays no
// part). A restore never moves to the other transport once it chose one; its files go over HTTP when HTTP answers.
func chooseRestore(f Facts) (Path, string) {
	why := transferBlocker(f, "restore")
	switch {
	case why == "" && httpFailing(f):
		return MQTT, "over MQTT (FileUpload), because " + httpFailText(f.HTTP)
	case why == "" && !f.MQTT.ConnChangedSince.IsZero():
		return HTTP, "over the device's HTTP API (restore upload), because " + connChangedText(f.MQTT)
	case why == "" && !f.MQTT.ReadsUnansweredSince.IsZero():
		return HTTP, "over the device's HTTP API (restore upload), because " + unansweredText(f.MQTT)
	case why == "":
		return MQTT, "over MQTT first (FileUpload; the device is online on TasmoFleet's broker and its topic is its own), " +
			"the backup's files over HTTP when HTTP answers; a backup that changes the device's MQTT connection asks first only when HTTP does not answer"
	case !httpFailing(f):
		return HTTP, "over the device's HTTP API (restore upload), because a restore over MQTT is not possible: " + why
	}
	return None, noPath(httpFailText(f.HTTP), "a restore over MQTT is not possible: "+why)
}

func chooseUpdate(f Facts) (Path, string) {
	push := f.Request.Delivery == model.DeliveryPush
	if push && !httpFailing(f) {
		if hp := f.Request.HTTPPush; hp == PushHTTP || hp == PushSafeboot {
			return hp, hp.Describe()
		}
	}
	if !httpFailing(f) {
		if why := fwBlocker(f); why == "" {
			path, text := chooseDownload(f)
			if push && f.Request.HTTPPushNote != "" {
				text += "; no upload: " + f.Request.HTTPPushNote
			}
			return path, text
		}
	}
	// HTTP is failing, or the device cannot fetch over HTTP's download.
	mqtt := mqttBlocker(f, true)
	if push && httpFailing(f) && mqtt == "" {
		if up := uploadBlocker(f); up == "" {
			return PushMQTT, PushMQTT.Describe() + ", because " + httpFailText(f.HTTP)
		} else if fw := fwBlocker(f); fw == "" {
			return DownloadMQTT, DownloadMQTT.Describe() + "; no upload over MQTT: " + up
		}
	}
	if mqtt == "" && fwBlocker(f) == "" {
		return DownloadMQTT, DownloadMQTT.Describe()
	}
	// Nothing works. Say what is wrong with each way.
	var httpWhy string
	if httpFailing(f) {
		httpWhy = httpFailText(f.HTTP)
	} else {
		httpWhy = fwBlocker(f)
	}
	parts := []string{httpWhy}
	if fw := fwBlocker(f); fw != "" && httpFailing(f) {
		parts = append(parts, fw)
	}
	if mqtt != "" {
		parts = append(parts, "MQTT is not usable: "+mqtt)
	}
	if push && httpFailing(f) {
		if up := uploadBlocker(f); up != "" {
			parts = append(parts, "no upload over MQTT: "+up)
		}
	}
	text := noPath(parts...)
	return None, text
}

// chooseDownload is the download of an update while HTTP is not failing and the device reaches the firmware server
// (#608). OtaUrl and Upgrade 1 are writes: the path is the one chooseWrites gives, so the two can never drift apart;
// only the sentence is the update's own. The device fetches the image from TasmoFleet's firmware server over HTTP
// either way. They need no MqttPassword. The job pins the path for its whole life (ota.pinned), so the sentence says
// so instead of a fallback.
func chooseDownload(f Facts) (Path, string) {
	if w, _ := chooseWrites(f); w == MQTT {
		return DownloadMQTT, DownloadMQTT.Describe() + "; OtaUrl and Upgrade go to the device's own topic on TasmoFleet's broker, " +
			"where it is online, and the update stays on MQTT until it ends"
	}
	viaHTTP := DownloadHTTP.Describe() + "; the commands go over HTTP because "
	switch why := mqttBlocker(f, true); {
	case why != "":
		return DownloadHTTP, viaHTTP + "MQTT is not usable: " + why
	case !f.MQTT.ConnChangedSince.IsZero():
		return DownloadHTTP, viaHTTP + connChangedText(f.MQTT)
	}
	// The other exception of chooseWrites (a new one there needs its sentence here; the path follows it anyway).
	return DownloadHTTP, viaHTTP + unansweredText(f.MQTT)
}

// uploadBlocker says why the upload over MQTT (FileUpload Type 1) cannot be used, "" when it can.
// The caller has checked mqttBlocker already.
func uploadBlocker(f Facts) string {
	switch {
	case !f.Request.AllowMQTTUpload:
		return "upload over MQTT is not allowed for this update (experimental)"
	case !f.Request.MQTTUploadFit.OK:
		if f.Request.MQTTUploadFit.Reason == "" {
			return fmt.Sprintf("the image does not fit for an upload over MQTT (needs %d KB, %d KB room)", f.Request.MQTTUploadFit.NeedKB, f.Request.MQTTUploadFit.RoomKB)
		}
		return f.Request.MQTTUploadFit.Reason
	}
	return passwordBlocker(f, "upload firmware")
}

// connChangedText says why HTTP carries the commands after a change of the device's MQTT connection.
func connChangedText(m MQTTFacts) string {
	return "TasmoFleet changed the device's MQTT connection (at " + m.ConnChangedSince.UTC().Format("15:04 UTC") +
		") and the device may not be back on the broker; MQTT is tried again after a few minutes"
}

// topicTakenText says why the device's commands do not go to its MQTT topic while another device answers there (#852).
func topicTakenText(m MQTTFacts) string {
	return "another device answers on the device's MQTT topic " + m.Topic + " (since " + m.TopicTakenSince.UTC().Format("2006-01-02 15:04 UTC") +
		"), so a command there would reach it too; MQTT is used again once only this device answers there"
}

// unansweredText says why HTTP carries an action while the device's MQTT commands go unanswered.
func unansweredText(m MQTTFacts) string {
	return "a command over MQTT got no answer from the device (since " + m.ReadsUnansweredSince.UTC().Format("15:04 UTC") +
		"); MQTT is tried again after a few minutes"
}

// othersUnreadNamed is how many of the devices othersUnreadText names; the rest are counted, so the sentence stays
// short on a narrow screen.
const othersUnreadNamed = 3

// othersUnreadText says which other devices hold this device's writes and transfers back (#812): their GroupTopic is
// unread, so a message to this device's topic could reach them.
func othersUnreadText(names []string) string {
	if len(names) == 1 {
		return "the group topic of " + names[0] + " has not been read yet, so a message to this device's topic could reach it; refresh its identity"
	}
	shown := names
	more := ""
	if len(names) > othersUnreadNamed {
		shown = names[:othersUnreadNamed]
		more = fmt.Sprintf(" and %d more devices", len(names)-othersUnreadNamed)
		if len(names)-othersUnreadNamed == 1 {
			more = " and 1 more device"
		}
	}
	list := strings.Join(shown[:len(shown)-1], ", ") + " and " + shown[len(shown)-1]
	if more != "" {
		list = strings.Join(shown, ", ") + more
	}
	return "the group topics of " + list + " have not been read yet, so a message to this device's topic could reach them; refresh their identity"
}

// MQTTBlocker is mqttBlocker for the router: why the device is not usable over MQTT right now ("" when
// it is). strict is the rule of writes and transfers. The router uses it for calls whose path a job
// pinned, where Choose (which answers HTTP while HTTP works) is not the question.
func MQTTBlocker(f Facts, strict bool) string { return mqttBlocker(f, strict) }
