---
section: Upgrade notes
---
The titles of the update messages changed: a finished update now reads "Job TX-0041: 11 ok, 1 failed" instead of "Update job #41: 11 ok, 1 failed", and the delivery status reads "job TX-0041 (partial)". This is the title of a webhook, ntfy or Gotify message and the subject of the e-mail, so a filter or rule that matches the old text needs the new one. The link in the message and the JSON fields stay as they were. The switch in Settings, Notifications is now called "Update finished" (#1107)
