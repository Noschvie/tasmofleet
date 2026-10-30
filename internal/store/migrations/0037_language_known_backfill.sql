-- Before 0.20 fw_language was written only by fwselect.Detect from the HTTP main page, and identity_at is
-- set only by an HTTP probe. Every such identity read its language from the page, so it is known;
-- 0036 added the flag with default 0, which would make Select refuse them all (D10). An identity read
-- over MQTT (identity_at NULL) stays unknown. Running jobs carry the identity in identity_before and
-- are resumed with it, so they get the flag too.
UPDATE devices SET identity_lang_known = 1 WHERE COALESCE(identity_at, 0) > 0;
UPDATE ota_jobs SET identity_before = json_set(identity_before, '$.LanguageKnown', json('true'))
  WHERE json_valid(identity_before) AND json_type(identity_before) = 'object';
