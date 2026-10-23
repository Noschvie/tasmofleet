-- The direct admin listener is gone (2026-10-19). Its sessions were valid only
-- there; without the listener check the main listener would accept the same
-- cookie value, so they are deleted. The listener column stays (append-only).
DELETE FROM sessions WHERE listener='admin';
