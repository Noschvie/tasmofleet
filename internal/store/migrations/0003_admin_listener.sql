-- Direct admin listener (H-1): a session records the listener it was created
-- on and is accepted only there, so a session stolen from one origin is
-- useless on the other. Existing sessions belong to the main listener.
ALTER TABLE sessions ADD COLUMN listener TEXT NOT NULL DEFAULT 'main' CHECK (listener IN ('main','admin'));
