---
section: Fixed
---
The device console, the audit log and the bulk command preview now show the value of `GroupTopic1` to `GroupTopic4` like they show `Topic` (`GroupTopic2 tf-livetest` instead of `GroupTopic2 ***`), so you can see what you are about to send and what was sent; a login pasted into it (`user:pass@`) is still masked, the command stays admin-only, and the console no longer names the transport for it before sending (#1046).
