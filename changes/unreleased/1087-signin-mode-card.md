---
section: Fixed
---
The Sign-in settings tell the truth about the active mode: behind a proxy the mode reads "Sign-in is handled by your proxy (header)" instead of the raw value, the Identity provider card says "Only used when sign-in mode is Single sign-on." and is inactive unless that mode is on (also when the environment fixes the mode, and without JavaScript), the Roles and claims card is inactive only with local accounts, and a mode card with nothing to change no longer says "Asks you to confirm" (#1087)
