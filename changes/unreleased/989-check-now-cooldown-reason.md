---
section: Fixed
---
Run now and Check now say when the per-device read cooldown (5 minutes) left a device alone and from when it can be read again, and the cooldown counts only a read that delivered every setting the device's rules judge: an MQTT message, a Make compliant check or a half-failed read no longer holds a check back or pushes it back, so a rule no longer stays "Not judged" because of an unrelated read. A device that never returns one judged setting is now read on every Run now (at most once a minute) instead of every 5 minutes (#989).
