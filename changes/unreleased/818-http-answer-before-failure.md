---
section: Fixed
---
The Reachability tile no longer reads "HTTP Reachable" from an answer that is older than the last failed HTTP attempt: after the ten minutes in which that failure steers commands to MQTT, it says "Not checked lately" until HTTP answers again, instead of "Reachable" for up to five more minutes (#818)
