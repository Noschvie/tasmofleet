---
section: Fixed
---

A device running a self-compiled safeboot image, whose version reads `16.0.0(safeboot)` with no `release-` or hash prefix, is now identified as safeboot over the main page, over INFO1 and in an update job, like the official image. Before, it stayed "unknown hardware" and a job through it stopped. Every other bare tag (`tasmota32`, `solo1`, `minimal`) still stays unknown and needs an explicit variant choice (#584)
