---
section: Fixed
---
Backups: the answer to Back up now and Retry (a refusal during the cooldown, a failed backup, the API turned off, a missing MQTT password) now appears directly under the bar of the Backups tab, under the empty card when there are no backups, and the page scrolls to it. With a long list it used to land below "Recent restores", off-screen. The wait in a refusal rounds up to the next full second, so it never says "possible in 0 s" (#994, thanks @Noschvie).
