---
section: Changed
---
The CI revert check no longer passes just because the tests do not build without the change: a package that does not build is run again with the PR's API kept and only the behaviour reverted, and a test has to fail on behaviour. A feature whose tests pass either way now fails the check (opt out with `revert-check: skip - <reason>`), and a change the check cannot rewrite is reported as inconclusive, never as a pass (#681)
