---
section: Fixed
---
The Devices list no longer cuts what it shows: the Address column is wide enough for a 15-character address or a 5-digit port with its link icon (#906), and in a saved layout a column that is too narrow for its own content (Last restart cut the end of its date) takes the room it lacks from columns that have more than they need, while the table keeps its width and a column you resize keeps the width you give it while you work on the page (#912). The tooltip of the OTA URL no longer covers the row above; that was fixed with the tooltip placement of #1073, and a test now guards it (#908)
