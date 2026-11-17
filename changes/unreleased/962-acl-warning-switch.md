---
section: Changed
---
The red broker ACL notice can be hidden with one option, "Hide the broker ACL warning" on System, MQTT (the old "This broker has an ACL" checkbox, reworded: TasmoFleet cannot see an ACL, and the option no longer says the broker has one). Its hint says what it means: every client of the broker can read device backups, passwords included, and the warning comes back when you change the broker. The notice's second line now reads "Hide it under System → MQTT", and for admins "Hide it" is a link that scrolls to the option and highlights it (#962)
