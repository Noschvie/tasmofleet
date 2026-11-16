---
section: Fixed
---
A device fine on MQTT whose HTTP failed once while it restarted no longer shows "cannot reach the device over HTTP" for ten minutes: opening its page checks HTTP again, even when the last HTTP answer or the last probe is only minutes old, because both came before the failure (#919)
