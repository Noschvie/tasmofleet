---
section: Changed
---
Filter chips and tags look and size the same on every page: a chip is a 28 px pill (44 px on a phone) and a tag an outlined label, where eight different styles drew them before (34 px or 28 px, a 7 px radius or a pill, a selected look that was inverted on some pages and tinted on others). The chip in use is tinted, never inverted, on the device list, the Backups and Policy filters, Add devices, the policy rule's scope, the weekdays of a schedule and the update's quick picks. The source of a discovered device on Add devices ("MQTT discovery", "Network scan") is a tag instead of an accent badge, because it says where a device came from, not what state it is in (#1095)
