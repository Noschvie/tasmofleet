---
section: Changed
---
Updates stop a device that runs firmware older than 10.0.0.3 at Tasmota 16.0.0, on every chip family: later releases no longer migrate its settings. The update preview says why and offers a separate preview of 16.0.0 for exactly those devices; the safety check refuses anything newer for it. Firmware older than 8.1.0 is refused anything after 8.5.1, 16.0.0 included, because Tasmota no longer migrates it and 16.0.0 would reset its configuration: such a device is offered no step and takes 8.5.1 first (Tasmota's migration path, from an uploaded image). The firmware index keeps 16.0.0 from both sources after newer releases are out (#892)
