# TasmoAdmin sample data

Synthetic files in the formats described in docs/research/tasmoadmin-data-formats.md.
Row 2 has a fake `enc:v1:` password (not decryptable), row 3 is a legacy 12-column
row with a plaintext password, row 4 a 7-column row with a backslash-escaped quote.
The bcrypt hash in MyConfig.json is not a real password hash.
