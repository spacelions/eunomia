CREATE TABLE IF NOT EXISTS connector_executions (
    tenant_id       TEXT        NOT NULL,
    idempotency_key TEXT        NOT NULL,
    run_id          TEXT        NOT NULL DEFAULT '',
    connector_type  TEXT        NOT NULL,
    operation       TEXT        NOT NULL,
    risk_tier       TEXT        NOT NULL CHECK (risk_tier IN ('write', 'irreversible')),
    status          TEXT        NOT NULL CHECK (status IN ('in_flight', 'succeeded', 'failed')),
    response        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    error_message   TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, idempotency_key)
);

--bun:split

CREATE INDEX IF NOT EXISTS idx_connector_executions_run
    ON connector_executions (tenant_id, run_id, created_at ASC)
    WHERE run_id <> '';
