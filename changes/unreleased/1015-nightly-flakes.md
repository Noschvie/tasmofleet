---
section: Fixed
---
Refreshing a device's identity after several refreshes in a row no longer waits behind a pile of identical background reads of the network facts: a device waits for at most one such read (#1015).
