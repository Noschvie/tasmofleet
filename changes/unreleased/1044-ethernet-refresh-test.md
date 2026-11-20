---
section: Fixed
---
The test of the Ethernet card (subnet mask and DNS after Refresh identity) could pass although the refresh read nothing, because earlier tests had already stored the same values: the simulated device now reports other values right before the refresh, and the test expects exactly those. No change to the app itself (#1044)
