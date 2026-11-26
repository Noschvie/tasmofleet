---
section: Fixed
---
Keyboard focus no longer ends under a pinned column of a table that scrolls sideways: Tab onto a sort link, a resize handle or a link in a column that has scrolled under the pinned ones now scrolls the table just far enough to show it with its focus ring, while Tab onto a pinned cell leaves the table where it is (#1275). The filter chips and tabs on a phone do the same inside their own strip: a focused chip is centred, so neither a sliver at the edge nor the fade cuts it (#1133, #1275)