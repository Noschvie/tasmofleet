---
section: Fixed
---
The nightly test of the Home Assistant app no longer fails at random when the Supervisor names the test backup with a slug of only digits: the test took the slug from the YAML output of the `ha` CLI, which quotes such a slug, and asked for a backup that does not exist. No change to the app itself (#833)
