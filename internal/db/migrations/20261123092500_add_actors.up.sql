CREATE TABLE IF NOT EXISTS actors (
    tenant_id  TEXT        NOT NULL,
    user_id    TEXT        NOT NULL,
    name       TEXT        NOT NULL,
    email      TEXT        NOT NULL,
    roles      JSONB       NOT NULL DEFAULT '[]'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, user_id)
);

--bun:split

CREATE UNIQUE INDEX IF NOT EXISTS idx_actors_tenant_email ON actors (tenant_id, LOWER(email));

--bun:split

CREATE INDEX IF NOT EXISTS idx_actors_roles_gin ON actors USING GIN (roles);

--bun:split

CREATE TABLE IF NOT EXISTS actor_identities (
    tenant_id   TEXT        NOT NULL,
    user_id     TEXT        NOT NULL,
    provider    TEXT        NOT NULL,
    external_id TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, user_id, provider),
    FOREIGN KEY (tenant_id, user_id) REFERENCES actors (tenant_id, user_id) ON DELETE CASCADE
);

--bun:split

CREATE UNIQUE INDEX IF NOT EXISTS idx_actor_identities_lookup
    ON actor_identities (tenant_id, provider, external_id);
