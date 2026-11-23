CREATE TABLE IF NOT EXISTS playbooks (
    tenant_id    TEXT        NOT NULL,
    playbook_id  TEXT        NOT NULL,
    version      INTEGER     NOT NULL CHECK (version >= 1),
    name         TEXT        NOT NULL,
    description  TEXT        NOT NULL DEFAULT '',
    meta         JSONB       NOT NULL,
    raw_markdown TEXT        NOT NULL,
    content_hash TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, playbook_id, version)
);

--bun:split

CREATE OR REPLACE FUNCTION prevent_playbook_update()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'playbooks rows are immutable: cannot update %/% v%', OLD.tenant_id, OLD.playbook_id, OLD.version;
END;
$$ LANGUAGE plpgsql;

--bun:split

DROP TRIGGER IF EXISTS trg_playbooks_immutable ON playbooks;

--bun:split

CREATE TRIGGER trg_playbooks_immutable
    BEFORE UPDATE ON playbooks
    FOR EACH ROW
    EXECUTE FUNCTION prevent_playbook_update();

--bun:split

CREATE TABLE IF NOT EXISTS playbook_drafts (
    tenant_id    TEXT        NOT NULL,
    playbook_id  TEXT        NOT NULL,
    raw_markdown TEXT        NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, playbook_id)
);

--bun:split

CREATE TABLE IF NOT EXISTS case_runs (
    tenant_id            TEXT        NOT NULL,
    run_id               TEXT        NOT NULL,
    playbook_id          TEXT        NOT NULL,
    playbook_version     INTEGER     NOT NULL,
    temporal_workflow_id TEXT        NOT NULL DEFAULT '',
    status               TEXT        NOT NULL CHECK (status IN (
        'pending',
        'running',
        'waiting_human',
        'waiting_approval',
        'completed',
        'escalated',
        'failed',
        'cancelled'
    )),
    dry_run              BOOLEAN     NOT NULL DEFAULT FALSE,
    inputs               JSONB       NOT NULL DEFAULT '{}'::jsonb,
    completed_tools      JSONB       NOT NULL DEFAULT '[]'::jsonb,
    turn_count           INTEGER     NOT NULL DEFAULT 0 CHECK (turn_count >= 0),
    result               JSONB       NOT NULL DEFAULT '{}'::jsonb,
    error_message        TEXT        NOT NULL DEFAULT '',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at           TIMESTAMPTZ,
    completed_at         TIMESTAMPTZ,
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, run_id),
    FOREIGN KEY (tenant_id, playbook_id, playbook_version)
        REFERENCES playbooks (tenant_id, playbook_id, version) ON DELETE RESTRICT
);

--bun:split

CREATE INDEX IF NOT EXISTS idx_case_runs_tenant_status
    ON case_runs (tenant_id, status, created_at DESC);

--bun:split

CREATE INDEX IF NOT EXISTS idx_case_runs_tenant_playbook
    ON case_runs (tenant_id, playbook_id, playbook_version);

--bun:split

CREATE TABLE IF NOT EXISTS interaction_tasks (
    tenant_id         TEXT        NOT NULL,
    task_id           TEXT        NOT NULL,
    run_id            TEXT        NOT NULL,
    kind              TEXT        NOT NULL CHECK (kind IN ('question', 'approval')),
    status            TEXT        NOT NULL CHECK (status IN (
        'open',
        'submitted',
        'approved',
        'rejected',
        'expired',
        'cancelled'
    )),
    recipient_user_id TEXT        NOT NULL DEFAULT '',
    recipient_email   TEXT        NOT NULL,
    token_hash        TEXT        NOT NULL,
    prompt_payload    JSONB       NOT NULL DEFAULT '{}'::jsonb,
    response_payload  JSONB       NOT NULL DEFAULT '{}'::jsonb,
    delivery_meta     JSONB       NOT NULL DEFAULT '{}'::jsonb,
    responded_by      TEXT        NOT NULL DEFAULT '',
    expires_at        TIMESTAMPTZ NOT NULL,
    responded_at      TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, task_id),
    FOREIGN KEY (tenant_id, run_id)
        REFERENCES case_runs (tenant_id, run_id) ON DELETE CASCADE
);

--bun:split

CREATE UNIQUE INDEX IF NOT EXISTS idx_interaction_tasks_token_hash
    ON interaction_tasks (token_hash);

--bun:split

CREATE INDEX IF NOT EXISTS idx_interaction_tasks_run
    ON interaction_tasks (tenant_id, run_id, created_at ASC);

--bun:split

CREATE INDEX IF NOT EXISTS idx_interaction_tasks_open_expiry
    ON interaction_tasks (status, expires_at)
    WHERE status = 'open';
