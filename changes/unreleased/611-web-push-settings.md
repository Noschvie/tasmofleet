---
section: Added
---
Web Push, in the browser: Settings → Notifications has a Web Push card. A browser subscribes with one click (the permission question comes from the button), sends itself a test and unsubscribes again. The card lists every subscribed browser with its owner, push service and last delivery, and any admin can test or remove any of them. Over plain HTTP, where browsers refuse push, the page says that HTTPS or localhost is needed instead of offering the button. Pushes now show with no TasmoFleet tab open. New migration: a label per subscription (#611, #541)
