# Synthetic Tasmota config dump

`synthetic-15.6.0.3.dmp` is a 4096-byte Tasmota settings dump (format 15.6.0.3)
that decode-config v16.0.0 accepts. It was **not** captured from a device: it
contains only the fake values in `restore.json` (reserved `.invalid` host, obvious
dummy passwords). Never commit a real dump here.

How it was made (decode-config v16.0.0 PyInstaller build, `$DC`):

    python3 -I make_base.py base.dmp           # blank settings, version + CRC32 only
    cp base.dmp synthetic-15.6.0.3.dmp
    $DC -s synthetic-15.6.0.3.dmp -i restore.json   # restore rewrites the -s file in place

Expected decoded values are asserted by `decoder_integration_test.go`.

`decoded.json` is the decode-config output for that dump with `header.env`
removed (as the backup service stores it). It feeds the diff redaction tests.

`synthetic-secrets-15.6.0.3.dmp` is the same dump after restoring
`restore-secrets.json`: a fake `FAKE-SECRET-*` (or recognisable number) value in
every secret field the diff must redact, plus credentials in `ota_url` and a
rule. `decoded-secrets.json` is its decode-config output (header.env removed).
Nothing in these files is real.
