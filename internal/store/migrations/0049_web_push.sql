-- 0049_web_push: Web Push, part 1 (#610): the VAPID key and the subscriptions of the browsers.
--
-- push_vapid holds the one P-256 key pair TasmoFleet signs its pushes with (RFC 8292). private_key is the raw
-- private scalar, base64url, sealed with secretbox like the other secrets, so the database alone does not give
-- it away. One row at most (id = 1); it is made on the first start and never rotated. When it cannot be opened
-- (secret.key replaced) the store replaces it and deletes every subscription in the same transaction: the push
-- services refuse a push signed with a key the subscription was not made for.
CREATE TABLE push_vapid (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    private_key TEXT    NOT NULL,
    created_at  INTEGER NOT NULL
);

-- push_subscriptions is one row per browser (the endpoint the push service gave it). p256dh and auth are the
-- browser's keys for the payload encryption (RFC 8291), base64url as the browser gives them; without the VAPID
-- private key they send nothing. The owner (sign-in method and subject) is the admin who subscribed; a push goes
-- only to a row whose owner is an admin at the time of the event. last_ok_at and last_error are the outcome of
-- the last push to the row (0 and '' until then).
CREATE TABLE push_subscriptions (
    endpoint      TEXT    PRIMARY KEY,
    p256dh        TEXT    NOT NULL,
    auth          TEXT    NOT NULL,
    owner_method  TEXT    NOT NULL,
    owner_subject TEXT    NOT NULL,
    created_at    INTEGER NOT NULL,
    last_ok_at    INTEGER NOT NULL DEFAULT 0,
    last_error    TEXT    NOT NULL DEFAULT ''
) WITHOUT ROWID;
