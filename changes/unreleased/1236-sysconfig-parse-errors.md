---
section: Fixed
---
System settings and the setup mark every refused setting, not only the first one: a setting that cannot be read at all (for example "abc" as an interval) or two intervals below 1s no longer hide each other, so the summary "Fix 2 fields. Nothing was saved yet." appears when two fields are refused. A wrong broker port or TLS value is still reported on its own (#1236)
