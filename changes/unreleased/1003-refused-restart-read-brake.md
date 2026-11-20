---
section: Fixed
---
A restart read whose identity check was refused because another device answers at the device's address no longer lets every further restart read the identity again: the next restart's identity and data read waits 10 minutes after the refusal. Restart messages can be published by any client on the broker, so a refusal that lasts no longer turns each of them into a full identity read. The first refusal by minimal firmware in an hour does not wait, so the boot into the new firmware after an update is still read in full at once (#1003).
