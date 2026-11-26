---
section: Fixed
---
Rules stored by `tasmofleet settings import` while the server runs now apply at once: Run now and the background reads judge the stored rules, so a new rule no longer stays "?" with its devices held back as "read recently". The server used to learn of a rule change only from the Policy page's own editor (#1309)
