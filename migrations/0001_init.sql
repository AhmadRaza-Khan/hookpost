CREATE TABLE IF NOT EXISTS hookpost_schema_version (
    version     INT PRIMARY KEY,
    applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS hookpost_endpoints (
    id                    TEXT PRIMARY KEY,
    owner_id              TEXT NOT NULL,
    url                   TEXT NOT NULL,
    description           TEXT NOT NULL DEFAULT '',
    secret                TEXT NOT NULL,
    old_secret            TEXT,
    old_secret_expires_at TIMESTAMPTZ,
    event_types           TEXT[] NOT NULL DEFAULT '{}',  -- empty = all events
    disabled              BOOLEAN NOT NULL DEFAULT false,
    disabled_reason       TEXT,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS hookpost_endpoints_owner
    ON hookpost_endpoints (owner_id) WHERE NOT disabled;

CREATE TABLE IF NOT EXISTS hookpost_messages (
    id          TEXT PRIMARY KEY,             -- becomes the webhook-id header
    owner_id    TEXT NOT NULL,
    event_type  TEXT NOT NULL,
    payload     BYTEA NOT NULL,               -- exact bytes we sign and send
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS hookpost_messages_owner_created
    ON hookpost_messages (owner_id, created_at DESC, id DESC);

CREATE TABLE IF NOT EXISTS hookpost_deliveries (
    id               BIGSERIAL PRIMARY KEY,
    message_id       TEXT NOT NULL REFERENCES hookpost_messages(id) ON DELETE CASCADE,
    endpoint_id      TEXT NOT NULL REFERENCES hookpost_endpoints(id) ON DELETE CASCADE,
    status           TEXT NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending','succeeded','dead')),
    attempts         INT  NOT NULL DEFAULT 0,
    next_attempt_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_status_code INT,
    last_error       TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (message_id, endpoint_id)
);
-- the dispatcher's hot path: only pending rows, ordered by due time
CREATE INDEX IF NOT EXISTS hookpost_deliveries_due
    ON hookpost_deliveries (next_attempt_at) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS hookpost_deliveries_endpoint_status
    ON hookpost_deliveries (endpoint_id, status);

CREATE TABLE IF NOT EXISTS hookpost_attempts (
    id            BIGSERIAL PRIMARY KEY,
    delivery_id   BIGINT NOT NULL REFERENCES hookpost_deliveries(id) ON DELETE CASCADE,
    attempt       INT NOT NULL,
    status_code   INT,
    error         TEXT,
    response_body TEXT,                       -- truncated to MaxResponseBytes
    duration_ms   INT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS hookpost_attempts_delivery
    ON hookpost_attempts (delivery_id);

INSERT INTO hookpost_schema_version (version) VALUES (1) ON CONFLICT DO NOTHING;
