CREATE TABLE IF NOT EXISTS tenant_connectors (
    tenant_id      TEXT        NOT NULL,
    connector_type TEXT        NOT NULL,
    enabled        BOOLEAN     NOT NULL DEFAULT FALSE,
    settings       JSONB       NOT NULL DEFAULT '{}'::jsonb,
    resources      JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, connector_type)
);

--bun:split

CREATE TABLE IF NOT EXISTS tenant_secrets (
    tenant_id      TEXT        NOT NULL,
    connector_type TEXT        NOT NULL,
    secret_key     TEXT        NOT NULL,
    key_id         TEXT        NOT NULL,
    encrypted_dek  BYTEA       NOT NULL,
    nonce          BYTEA       NOT NULL,
    ciphertext     BYTEA       NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, connector_type, secret_key)
);

--bun:split

CREATE INDEX IF NOT EXISTS idx_tenant_secrets_key_id ON tenant_secrets (key_id);
