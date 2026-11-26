---
section: Changed
---
Every gap, padding and margin in the stylesheet comes from the spacing tokens, and a test now fails when one is written as px or rem. The space between sibling cards is one value, 16 px, on every page: it was 12 px from the tile row to the Relays card and from there to the cards below on the device overview, 16 px in the card stacks, and 20 to 24 px on the new rule page and the rule page (a card's own margin added to the gap of the grid that holds it). Spacing written in rem moves by at most half a pixel to the nearest token, and the unused rule page layout styles are gone (#1101)
