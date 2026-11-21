CREATE TABLE IF NOT EXISTS tasks (
 id text PRIMARY KEY, owner_id text NOT NULL, family text NOT NULL, network text NOT NULL,
 sender text NOT NULL, nonce text, payment text,
 state text NOT NULL, active text NOT NULL, outcome text NOT NULL DEFAULT '',
 winner text NOT NULL DEFAULT '', consuming text NOT NULL DEFAULT '', execution text NOT NULL DEFAULT '',
 reason text NOT NULL DEFAULT '', evidence blob, revision integer NOT NULL DEFAULT 1,
 forced boolean NOT NULL DEFAULT false, created integer NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000 AS INTEGER)),
 due integer NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000 AS INTEGER)), lease text, lease_until integer,
 UNIQUE(network,sender,nonce), UNIQUE(owner_id,network,payment)
);
CREATE INDEX IF NOT EXISTS tasks_due ON tasks(due) WHERE state NOT IN ('CLOSED','RESIGN_REQUIRED');
CREATE TABLE IF NOT EXISTS versions (
 network text NOT NULL, hash text NOT NULL, task text NOT NULL REFERENCES tasks(id),
 raw text NOT NULL, state text NOT NULL, blockhash text NOT NULL, intent text NOT NULL,
 context_slot integer NOT NULL, last_valid integer NOT NULL, auth blob NOT NULL,
 created integer NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000 AS INTEGER)), PRIMARY KEY(network,hash)
);
CREATE INDEX IF NOT EXISTS versions_task ON versions(task);
CREATE TABLE IF NOT EXISTS providers (
 id text PRIMARY KEY, provider text NOT NULL, network text NOT NULL, secret blob NOT NULL,
 enabled boolean NOT NULL, height integer NOT NULL, progressed integer NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000 AS INTEGER)),
 checked integer NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000 AS INTEGER)), healthy boolean NOT NULL DEFAULT true,
 failures int NOT NULL DEFAULT 0, level int NOT NULL DEFAULT 0, generation integer NOT NULL DEFAULT 0,
 cooldown integer, probe_until integer, last_error text NOT NULL DEFAULT '',
 config_version integer NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS deliveries (
 id text PRIMARY KEY, task text NOT NULL REFERENCES tasks(id), hash text NOT NULL,
 round text NOT NULL, provider text NOT NULL, endpoint text NOT NULL, state text NOT NULL,
 detail text NOT NULL DEFAULT '', started integer NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000 AS INTEGER)), latency integer NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS deliveries_task ON deliveries(task,started);
CREATE TABLE IF NOT EXISTS outbox (
 id text PRIMARY KEY, owner_id text NOT NULL, task text NOT NULL REFERENCES tasks(id),
 name text NOT NULL, revision integer NOT NULL, payload blob NOT NULL,
 due integer NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000 AS INTEGER)), sent integer,
 token text, attempts int NOT NULL DEFAULT 0, UNIQUE(task,name,revision)
);
CREATE INDEX IF NOT EXISTS outbox_due ON outbox(due) WHERE sent IS NULL;
CREATE TABLE IF NOT EXISTS force_audit (
 task text NOT NULL REFERENCES tasks(id), actor text NOT NULL, reason text NOT NULL, created integer NOT NULL DEFAULT (CAST(unixepoch('subsec')*1000 AS INTEGER))
);
