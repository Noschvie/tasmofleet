---
section: Security
---
A `Template` whose JSON has a `CMND` key (any letter case) is logged as `Template ***` in the audit log and the server log, is not kept in the console's command history, and its console confirmation names the way it goes to the device, like the commands it runs: before, a password inside its `CMND` (`WebPassword ...`) was kept there in clear. A `Backlog` with a part whose braces do not balance (`{` and `}`) is now refused as a malformed command: the device joins such a part with the next one, so a `;` inside a `Template`'s JSON hid its `CMND` from the masking and from the MQTT channel (#843)
