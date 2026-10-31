-- 0040_principal_custom_name: the display name a person chose for themselves on their Account page (#283).
-- principals.display_name stays the name captured at the last sign-in (the identity provider's claim, which
-- every sign-in overwrites); custom_name is a separate column so that overwrite can never wipe the choice.
-- NULL = none chosen, the sign-in name applies. Presentation only: the subject stays the identity.
ALTER TABLE principals ADD COLUMN custom_name TEXT;
