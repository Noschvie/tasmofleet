---
section: Changed
user: What's new lists one headline per change and shows the last five releases
---
Each release names its changes in one line each, without migration names, column names, issue numbers or credits, and keeps the full text behind a "Details" fold, so the page is short enough to read. A Breaking change is a warning at the top of its release. The page shows the newest five releases, and when more than five are new it says how many older ones the full changelog holds; the jump card lists the same five and the "Older releases" fold is gone. A changelog line gets its headline from an optional `user:` line in its fragment, else from its first clause (#1108)
