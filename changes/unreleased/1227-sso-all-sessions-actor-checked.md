---
section: Security
---
Ending all sessions of an SSO user now checks that the acting local admin is still an admin when the sessions are deleted, like ending the sessions of a local user does; a stale admin gets "Your account changed while the request ran" and nothing is deleted (#1227, follow-up of #1203)
