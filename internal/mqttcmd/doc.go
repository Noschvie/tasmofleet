// Package mqttcmd is the one place that sends Tasmota commands over MQTT and reads the answer
// (#79, #256). It sits on mqttbus and is a SAFETY package: it decides what may be published and
// which incoming message counts as the answer.
//
// Rules, each with a test:
//
//   - Never "Status 0" (any spelling cmdpolicy would read as 0), never a group topic, never a
//     topic with MQTT wildcards or separators. FileDownload, FileUpload and Upload are refused by Do:
//     they carry the device's MQTT password and have their own API (Download, see filedownload.go;
//     Upload, see upload.go). Upload shares Download's per-topic clean state (one FMqtt state on
//     the device), asserts the payload budget 1200-5-len(cmnd topic)-1 before every publish, and
//     sends "FileUpload 0" after any failure but the device's own "Error N". Download holds the topic for the whole transfer, sends one "?" per
//     Data message, checks Size, Id and MD5, aborts with "FileDownload 0" on any irregularity, and
//     never logs or returns a payload (the start carries the password, a minimal firmware echoes it).
//   - One command in flight per device topic (first come, first served) and at least Gap between
//     two publishes on the whole bus: the heap rule of Status queries applies on MQTT too.
//   - The reply listener is registered before the publish, so a fast device cannot answer into
//     the void (#272). The bus reports Connected() only after its subscriptions are in place.
//   - Retained messages are never answers. A reply is accepted only when it is the one the command
//     produces: STATUSn for "Status n", else a RESULT whose top-level keys name the command (or
//     {"Command":"Unknown"|"Error"} for that command). Button presses and other pollers are ignored.
//     A Command object is a failure, never data, also on a STATUS topic: a Status failure answers only
//     when its Input names that Status ("STATUS 9" or "STATUS9" for Status 9, a bare "STATUS" for
//     Status), on RESULT, on its own STATUSn or, with SetOption4 1, on stat/<t>/STATUS (#1209). An
//     Input that is no text or names another command or Status is someone else's failure.
//     A device with SetOption4 1 publishes that reply on stat/<t>/<COMMAND> instead of RESULT (the
//     name in upper case without its index, COMMAND for an unknown command, POWERn for a relay): it
//     is accepted only on the pending command's own name, with the same key rules (#632). Upgrade answers
//     on stat/<t>/UPGRADE, the topic of its download reports, and a failed Power3 on stat/<t>/POWER: only
//     the immediate Upgrade reply ("Version X from <url>", "1 or >X to upgrade", "Safeboot") and only the
//     Command Error answer there, never "Failed ..." or "Successful. Restarting" (#648).
//   - One rule (Reply) says how an answer is recognised, for reads and writes. A light write (Dimmer, Color,
//     Channel<n>, HSBColor, White, CT with an argument) is answered only by the light state every light change
//     publishes, so it is never taken as the answer: the channel publishes ConfirmRead (Status 11) after the write,
//     on the same topic, still holding it, and only that STATUS11 (or a Command Error/Unknown for the write)
//     answers, and only after the write's own response; the reply is the object inside StatusSTS, always scrubbed
//     (it carries the Wi-Fi name). A failure after the write was published is an unknown outcome; a write that
//     fails on its own after its Status 11 went out keeps the topic until that STATUS11 comes. A Backlog with a
//     light write gets Status 11 as its last part and needs its STATUS11, and a light part keeps its light state;
//     one too long for it is refused (#813).
//   - A Backlog has no merged reply (Tasmota v16 publishes nothing for the Backlog itself and one
//     RESULT per queued command): each queued command is collected on its own.
//   - A device whose last LWT is Offline is not published to (ErrUnreachable: nothing was sent).
//     No answer within ReplyWait is tasmota.ErrTimeout: the outcome is unknown and the channel
//     never resends.
//   - Replies to commands that set a credential (WebPassword, MqttPassword, ...) are scrubbed with
//     cmdpolicy.ScrubReply before they leave the package, because the device echoes the new
//     secret. No payload is ever logged here.
package mqttcmd
