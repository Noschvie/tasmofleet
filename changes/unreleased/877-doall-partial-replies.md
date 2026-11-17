---
section: Fixed
---

MQTT: a check that hears every device on a topic (the end of a "topic taken" mark, the restore's check before its upload) now fails when the MQTT channel closes inside its reply window, instead of taking the replies heard so far as all of them (#877)
