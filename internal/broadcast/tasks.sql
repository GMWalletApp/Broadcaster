CREATE TABLE IF NOT EXISTS tasks (
 id text PRIMARY KEY, owner_id text NOT NULL, family text NOT NULL, network text NOT NULL,
 sender text NOT NULL, nonce text, payment text,
 state text NOT NULL, active text NOT NULL, outcome text NOT NULL DEFAULT '',
 winner text NOT NULL DEFAULT '', consuming text NOT NULL DEFAULT '', execution text NOT NULL DEFAULT '',
 reason text NOT NULL DEFAULT '', evidence blob, revision integer NOT NULL DEFAULT 1,
 forced boolean NOT NULL DEFAULT false, created integer NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000 AS INTEGER)),
 due integer NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000 AS INTEGER)), lease text, lease_until integer,
 UNIQUE(owner_id,network,payment)
);
