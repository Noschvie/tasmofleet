---
section: Added
---
Web Push, server side: TasmoFleet makes its own VAPID key (sealed at rest with `secret.key`), stores one push subscription per browser for admins and pushes the same events as the browser notifications, encrypted for the browser (RFC 8291), so they can show with no TasmoFleet tab open. A subscription is removed when the push service says it is gone (404, 410) or its owner is no longer an admin (for single sign-on and header users: as recorded at their last sign-in). This is a new outbound HTTPS connection to the browser vendors' push services. The subscribe button in the browser follows in #611 (#610)
