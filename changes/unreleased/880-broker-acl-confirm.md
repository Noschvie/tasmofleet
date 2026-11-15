---
section: Added
---
System → MQTT has a checkbox "This broker has an ACL: clients read only their own topics (hides the ACL notice)", for admins. It hides the red broker ACL notice everywhere (console, bulk command and restore confirmations, the device's Back up button, Back up all now, the backup pages) for the broker it was given for: host, port and user. Change any of them and the notice returns. The notice now also says where to confirm (#880)
