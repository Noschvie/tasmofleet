# UI changes

Part of the working guide: start at [CLAUDE.md](../../CLAUDE.md).

**UI changes:** the agent makes screenshot pairs per screen (mockup and build where a mockup exists,
otherwise before and after; 1280 and 375 px, light and dark) and keeps them outside the repo. During design
phases these local before/after comparisons are what the maintainer looks at; they never hold work back.
Before merging, the coordinator compares them; every visible difference is fixed or listed in the PR with
the reason. Screenshots are never committed to any branch or embedded in a PR. "Tests pass" is not
acceptance for UI work.
