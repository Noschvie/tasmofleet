// Package mqttcmd is the one place that sends Tasmota commands over MQTT and reads the answer
// (#79, #256). It sits on mqttbus and is a SAFETY package: it decides what may be published and
// which incoming message counts as the answer.
//
// Rules, each with a test:
//
//   - Never "Status 0" (any spelling cmdpolicy would read as 0), never a group topic, never a
//     topic with MQTT wildcards or separators. FileDownload, FileUpload and Upload are refused by Do:
//     they carry the device's MQTT password and have their own API (Download, see filedownload.go;
//     the uploader comes later). Download holds the topic for the whole transfer, sends one "?" per
//     Data message, checks Size, Id and MD5, aborts with "FileDownload 0" on any irregularity, and
//     never logs or returns a payload (the start carries the password, a minimal firmware echoes it).
//   - One command in flight per device topic (first come, first served) and at least Gap between
//     two publishes on the whole bus: the heap rule of Status queries applies on MQTT too.
//   - The reply listener is registered before the publish, so a fast device cannot answer into
//     the void (#272). The bus reports Connected() only after its subscriptions are in place.
//   - Retained messages are never answers. A reply is accepted only when it is the one the command
//     produces: STATUSn for "Status n", else a RESULT whose top-level keys name the command (or
//     {"Command":"Unknown"|"Error"} for that command). Button presses and other pollers are ignored.
//   - A Backlog has no merged reply (Tasmota v16 publishes nothing for the Backlog itself and one
//     RESULT per queued command): each queued command is collected on its own.
//   - A device whose last LWT is Offline is not published to (ErrUnreachable: nothing was sent).
//     No answer within ReplyWait is tasmota.ErrTimeout: the outcome is unknown and the channel
//     never resends.
//   - Replies to commands that set a credential (WebPassword, MqttPassword, ...) are scrubbed with
//     cmdpolicy.ScrubReply before they leave the package, because the device echoes the new
//     secret. No payload is ever logged here.
package mqttcmd
