
CREATE TABLE IF NOT EXISTS ollama_models (
    id VARCHAR(255) PRIMARY KEY,
    model VARCHAR(512) NOT NULL UNIQUE,

    can_chat BOOLEAN NOT NULL DEFAULT false,
    can_stream BOOLEAN NOT NULL DEFAULT false,
    can_prompt BOOLEAN NOT NULL DEFAULT false,
    can_embed BOOLEAN NOT NULL DEFAULT false,
    context_length INT NOT NULL DEFAULT 0,

    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS llm_affinity_group (
    id VARCHAR(255) PRIMARY KEY,
    name VARCHAR(512) NOT NULL UNIQUE,
    purpose_type VARCHAR(512) NOT NULL,

    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS llm_backends (
    id VARCHAR(255) PRIMARY KEY,
    name VARCHAR(512) NOT NULL UNIQUE,
    base_url VARCHAR(512) NOT NULL,
    type VARCHAR(512) NOT NULL,

    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    UNIQUE(type, base_url)
);

CREATE TABLE IF NOT EXISTS llm_affinity_group_backend_assignments (
    group_id VARCHAR(255) NOT NULL REFERENCES llm_affinity_group(id) ON DELETE CASCADE,
    backend_id VARCHAR(255) NOT NULL REFERENCES llm_backends(id) ON DELETE CASCADE,
    PRIMARY KEY (group_id, backend_id),
    assigned_at TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS ollama_model_assignments (
    model_id VARCHAR(255) NOT NULL REFERENCES ollama_models(id) ON DELETE CASCADE,
    llm_group_id VARCHAR(255) NOT NULL REFERENCES llm_affinity_group(id) ON DELETE CASCADE,
    PRIMARY KEY (model_id, llm_group_id),

    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS job_queue_v2 (
    id VARCHAR(255) PRIMARY KEY,
    task_type VARCHAR(512) NOT NULL,
    payload JSONB NOT NULL,

    scheduled_for INT,
    valid_until INT,
    retry_count INT NOT NULL DEFAULT 0,
    created_at TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS entity_events (
    id VARCHAR(255) PRIMARY KEY,
    entity_id VARCHAR(255) NOT NULL,
    entity_type VARCHAR(255) NOT NULL,
    created_at TIMESTAMP NOT NULL,
    processed_at TIMESTAMP,
    error TEXT
);

CREATE TABLE IF NOT EXISTS kv (
    key VARCHAR(255) NOT NULL,
    workspace_id VARCHAR(255) NOT NULL DEFAULT '',
    value JSONB NOT NULL,

    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    PRIMARY KEY (key, workspace_id)
);

CREATE TABLE IF NOT EXISTS remote_hooks (
    id VARCHAR(255) PRIMARY KEY,
    name VARCHAR(255) NOT NULL UNIQUE,
    endpoint_url VARCHAR(512) NOT NULL,
    timeout_ms INT NOT NULL DEFAULT 5000,
    headers JSONB,
    properties BYTEA,
    inject_params_json JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL
);

ALTER TABLE remote_hooks ADD COLUMN IF NOT EXISTS body_properties BYTEA;
ALTER TABLE remote_hooks ADD COLUMN IF NOT EXISTS headers JSONB;
ALTER TABLE remote_hooks ADD COLUMN IF NOT EXISTS inject_params_json JSONB DEFAULT '{}';


CREATE INDEX IF NOT EXISTS idx_job_queue_v2_task_type ON job_queue_v2 USING hash(task_type);


CREATE OR REPLACE FUNCTION estimate_row_count(table_name TEXT)
RETURNS BIGINT AS $$
DECLARE
    result BIGINT;
BEGIN
    SELECT reltuples::BIGINT
    INTO result
    FROM pg_class
    WHERE relname = table_name;

    RETURN COALESCE(result, 0);
END;
$$ LANGUAGE plpgsql STABLE;

CREATE TABLE IF NOT EXISTS message_indices (
    id VARCHAR(255) PRIMARY KEY,
    identity VARCHAR(512) NOT NULL,
    workspace_id VARCHAR(255) NOT NULL DEFAULT '',
    name VARCHAR(255)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_message_indices_name
    ON message_indices (name, workspace_id)
    WHERE name IS NOT NULL;

CREATE TABLE IF NOT EXISTS messages (
    id VARCHAR(255),
    idx_id VARCHAR(255) NOT NULL REFERENCES message_indices(id) ON DELETE CASCADE,
    payload JSONB NOT NULL,
    added_at TIMESTAMP NOT NULL,
    PRIMARY KEY (id, idx_id)
);

CREATE INDEX IF NOT EXISTS idx_messages_idx_id ON messages (idx_id);
CREATE INDEX IF NOT EXISTS idx_messages_added_at ON messages (added_at);
CREATE INDEX IF NOT EXISTS idx_message_indices_identity ON message_indices (identity);

CREATE TABLE IF NOT EXISTS mcp_servers (
    id                      VARCHAR(255) PRIMARY KEY,
    name                    VARCHAR(255) NOT NULL UNIQUE,
    transport               VARCHAR(50)  NOT NULL DEFAULT 'sse',
    command                 TEXT,
    args_json               TEXT,
    url                     TEXT,
    auth_type               VARCHAR(50),
    auth_token              TEXT,
    auth_env_key            TEXT,
    connect_timeout_seconds INTEGER NOT NULL DEFAULT 30,
    created_at              TIMESTAMP NOT NULL,
    updated_at              TIMESTAMP NOT NULL
);
ALTER TABLE mcp_servers ADD COLUMN IF NOT EXISTS headers_json JSONB DEFAULT '{}';
ALTER TABLE mcp_servers ADD COLUMN IF NOT EXISTS inject_params_json JSONB DEFAULT '{}';
CREATE INDEX IF NOT EXISTS idx_mcp_servers_created_at ON mcp_servers(created_at);

CREATE TABLE IF NOT EXISTS plans (
    id         VARCHAR(255) PRIMARY KEY,
    name       VARCHAR(255) NOT NULL,
    workspace_id VARCHAR(255) NOT NULL DEFAULT '',
    goal       TEXT         NOT NULL,
    status     VARCHAR(50)  NOT NULL DEFAULT 'active',
    session_id VARCHAR(255),
    compiled_chain_json          TEXT,
    compiled_chain_id            VARCHAR(255),
    compile_executor_chain_id    VARCHAR(255),
    created_at TIMESTAMP    NOT NULL,
    updated_at TIMESTAMP    NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_plans_name_workspace ON plans (name, workspace_id);
ALTER TABLE plans ADD COLUMN IF NOT EXISTS compiled_chain_json TEXT;
ALTER TABLE plans ADD COLUMN IF NOT EXISTS compiled_chain_id VARCHAR(255);
ALTER TABLE plans ADD COLUMN IF NOT EXISTS compile_executor_chain_id VARCHAR(255);

CREATE TABLE IF NOT EXISTS plan_steps (
    id                    VARCHAR(255) PRIMARY KEY,
    plan_id               VARCHAR(255) NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
    ordinal               INT          NOT NULL,
    description           TEXT         NOT NULL,
    status                VARCHAR(50)  NOT NULL DEFAULT 'pending',
    execution_result      TEXT         NOT NULL DEFAULT '',
    executed_at           TIMESTAMP,
    summary               TEXT,
    chat_history_json     TEXT,
    summary_error         TEXT,
    last_failure_summary  TEXT,
    UNIQUE (plan_id, ordinal)
);
CREATE INDEX IF NOT EXISTS idx_plan_steps_plan_id ON plan_steps(plan_id);

-- plan_steps: typed-handover columns for existing DBs (see planstore/summary.go).
ALTER TABLE plan_steps ADD COLUMN IF NOT EXISTS summary              TEXT;
ALTER TABLE plan_steps ADD COLUMN IF NOT EXISTS chat_history_json    TEXT;
ALTER TABLE plan_steps ADD COLUMN IF NOT EXISTS summary_error        TEXT;
ALTER TABLE plan_steps ADD COLUMN IF NOT EXISTS last_failure_summary TEXT;

CREATE TABLE IF NOT EXISTS llm_model_registry (
    id          VARCHAR(255) PRIMARY KEY,
    name        VARCHAR(512) NOT NULL UNIQUE,
    source_url  VARCHAR(1024) NOT NULL,
    size_bytes  BIGINT NOT NULL DEFAULT 0,
    created_at  TIMESTAMP NOT NULL,
    updated_at  TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_llm_model_registry_created_at ON llm_model_registry(created_at);

