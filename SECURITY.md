# Security policy

## Reporting a vulnerability

Please report vulnerabilities **privately** through GitHub:
[Report a vulnerability](https://github.com/nofuturekid/tasmofleet/security/advisories/new)
(Security tab, "Report a vulnerability"). Do not open a public issue.

Include the version, how to reproduce it and the impact you see. This is a
single-maintainer project: expect a first answer within a week. Fixes ship as a
patch release, and the advisory is published once a fixed image is available.

## Supported versions

Only the latest release receives security fixes while the project is pre-1.0.

## Scope

TasmoFleet manages devices on a local network and holds their credentials, so
reports about authentication, authorisation, CSRF, SSRF towards devices,
credential exposure and firmware integrity are especially welcome. The Tasmota
firmware itself is out of scope; report those issues
[upstream](https://github.com/arendst/Tasmota).
