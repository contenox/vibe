-- SQLite-compatible schema for Contenox Local (single-file DB).
-- JSONB -> TEXT, BYTEA -> BLOB. No estimate_row_count (Postgres-only); callers must use COUNT(*) or avoid.

CREATE TABLE IF NOT EXISTS ollama_models (
    id VARCHAR(255) PRIMARY KEY,
    model VARCHAR(512) NOT NULL UNIQUE,

    can_chat BOOLEAN NOT NULL DEFAULT 0,
    can_stream BOOLEAN NOT NULL DEFAULT 0,
    can_prompt BOOLEAN NOT NULL DEFAULT 0,
    can_embed BOOLEAN NOT NULL DEFAULT 0,
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
    base_url VARCHAR(512) NOT NULL UNIQUE,
    type VARCHAR(512) NOT NULL,

    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS llm_affinity_group_backend_assignments (
    group_id VARCHAR(255) NOT NULL REFERENCES llm_affinity_group(id) ON DELETE CASCADE,
    backend_id VARCHAR(255) NOT NULL REFERENCES llm_backends(id) ON DELETE CASCADE,
    assigned_at TIMESTAMP NOT NULL,
    PRIMARY KEY (group_id, backend_id)
);

CREATE TABLE IF NOT EXISTS ollama_model_assignments (
    model_id VARCHAR(255) NOT NULL REFERENCES ollama_models(id) ON DELETE CASCADE,
    llm_group_id VARCHAR(255) NOT NULL REFERENCES llm_affinity_group(id) ON DELETE CASCADE,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    PRIMARY KEY (model_id, llm_group_id)
);

CREATE TABLE IF NOT EXISTS job_queue_v2 (
    id VARCHAR(255) PRIMARY KEY,
    task_type VARCHAR(512) NOT NULL,
    payload TEXT NOT NULL,

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
    key VARCHAR(255) PRIMARY KEY,
    value TEXT NOT NULL,

    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS remote_hooks (
    id VARCHAR(255) PRIMARY KEY,
    name VARCHAR(255) NOT NULL UNIQUE,
    endpoint_url VARCHAR(512) NOT NULL,
    timeout_ms INT NOT NULL DEFAULT 5000,
    headers TEXT,
    properties BLOB,
    inject_params_json TEXT NOT NULL DEFAULT '{}',
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL
);

-- SQLite does not support ADD COLUMN IF NOT EXISTS in older versions; skip if already present
-- (run once; columns may exist from a previous schema)
-- For fresh installs the table has headers/properties above. body_properties omitted for minimal local.

CREATE INDEX IF NOT EXISTS idx_job_queue_v2_task_type ON job_queue_v2(task_type);

-- Event-dispatched functions and triggers (used by Contenox CLI event dispatcher / Goja executor).
CREATE TABLE IF NOT EXISTS functions (
    name TEXT PRIMARY KEY,
    description TEXT,
    script_type TEXT NOT NULL,
    script TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS event_triggers (
    name TEXT PRIMARY KEY,
    description TEXT,
    listen_for_type TEXT NOT NULL,
    trigger_type TEXT NOT NULL,
    function_name TEXT NOT NULL REFERENCES functions(name) ON DELETE CASCADE,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS message_indices (
    id VARCHAR(255) PRIMARY KEY,
    identity VARCHAR(512) NOT NULL,
    name VARCHAR(255)  -- human-readable session name; NULL = unnamed
);

-- Partial unique index: only enforce uniqueness when name IS NOT NULL
CREATE UNIQUE INDEX IF NOT EXISTS idx_message_indices_name
    ON message_indices (name)
    WHERE name IS NOT NULL;

CREATE TABLE IF NOT EXISTS messages (
    id VARCHAR(255),
    idx_id VARCHAR(255) NOT NULL REFERENCES message_indices(id) ON DELETE CASCADE,
    payload TEXT NOT NULL,
    added_at TIMESTAMP NOT NULL,
    PRIMARY KEY (id, idx_id)
);

CREATE INDEX IF NOT EXISTS idx_messages_idx_id ON messages (idx_id);
CREATE INDEX IF NOT EXISTS idx_messages_added_at ON messages (added_at);
CREATE INDEX IF NOT EXISTS idx_message_indices_identity ON message_indices (identity);

CREATE INDEX IF NOT EXISTS idx_functions_created_at ON functions(created_at);
CREATE INDEX IF NOT EXISTS idx_event_triggers_created_at ON event_triggers(created_at);
CREATE INDEX IF NOT EXISTS idx_event_triggers_listen_for_type ON event_triggers(listen_for_type);
CREATE INDEX IF NOT EXISTS idx_event_triggers_function_name ON event_triggers(function_name);

CREATE TABLE IF NOT EXISTS plans (
    id VARCHAR(255) PRIMARY KEY,
    name VARCHAR(255) UNIQUE NOT NULL,
    goal TEXT NOT NULL,
    status VARCHAR(50) DEFAULT 'active', -- active | completed | archived
    session_id VARCHAR(255),             -- optional FK to message_indices
    compiled_chain_json          TEXT,
    compiled_chain_id            VARCHAR(255),
    compile_executor_chain_id    VARCHAR(255),
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS plan_steps (
    id VARCHAR(255) PRIMARY KEY,
    plan_id VARCHAR(255) REFERENCES plans(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL,
    description TEXT NOT NULL,
    status VARCHAR(50) DEFAULT 'pending', -- pending | completed | failed | skipped
    execution_result TEXT,                -- summary / error / full output
    executed_at TIMESTAMP,
    UNIQUE(plan_id, ordinal)
);

CREATE INDEX IF NOT EXISTS idx_plan_steps_plan ON plan_steps(plan_id, ordinal);

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
    headers_json            TEXT NOT NULL DEFAULT '{}',
    inject_params_json      TEXT NOT NULL DEFAULT '{}',
    created_at              TIMESTAMP NOT NULL,
    updated_at              TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_mcp_servers_created_at ON mcp_servers(created_at);

CREATE TABLE IF NOT EXISTS terminal_sessions (
    id VARCHAR(255) PRIMARY KEY,
    principal VARCHAR(512) NOT NULL,
    cwd TEXT NOT NULL,
    shell VARCHAR(512) NOT NULL,
    cols INT NOT NULL,
    rows INT NOT NULL,
    status VARCHAR(50) NOT NULL DEFAULT 'active',
    node_instance_id VARCHAR(255) NOT NULL,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_terminal_sessions_principal_created ON terminal_sessions (principal, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_terminal_sessions_node ON terminal_sessions (node_instance_id);

CREATE TABLE IF NOT EXISTS workspaces (
    id VARCHAR(255) PRIMARY KEY,
    principal VARCHAR(512) NOT NULL,
    name VARCHAR(255) NOT NULL,
    path TEXT NOT NULL,
    shell VARCHAR(512),
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    UNIQUE (principal, name)
);
CREATE INDEX IF NOT EXISTS idx_workspaces_principal_created ON workspaces (principal, created_at DESC);

-- libbus.SQLiteBus tables -----------------------------------------------

CREATE TABLE IF NOT EXISTS bus_events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    subject    TEXT    NOT NULL,
    data       BLOB    NOT NULL,
    created_at INTEGER NOT NULL DEFAULT (unixepoch('now'))
);
CREATE INDEX IF NOT EXISTS idx_bus_events_subject ON bus_events(subject, id);

CREATE TABLE IF NOT EXISTS bus_requests (
    id         TEXT    PRIMARY KEY,
    subject    TEXT    NOT NULL,
    data       BLOB    NOT NULL,
    created_at INTEGER NOT NULL DEFAULT (unixepoch('now'))
);
CREATE INDEX IF NOT EXISTS idx_bus_requests_subject ON bus_requests(subject, created_at);

CREATE TABLE IF NOT EXISTS bus_replies (
    request_id TEXT    PRIMARY KEY,
    data       BLOB    NOT NULL,
    created_at INTEGER NOT NULL DEFAULT (unixepoch('now'))
);

-- Incremental migrations — executed one-by-one by NewSQLiteDBManager so that
-- "duplicate column name" errors on already-upgraded databases are silently
-- skipped and the remaining statements still run.

-- remote_hooks columns added after initial release
ALTER TABLE remote_hooks ADD COLUMN headers             TEXT;
ALTER TABLE remote_hooks ADD COLUMN properties         BLOB;
ALTER TABLE remote_hooks ADD COLUMN inject_params_json TEXT NOT NULL DEFAULT '{}';

-- mcp_servers columns added after initial release
ALTER TABLE mcp_servers ADD COLUMN headers_json        TEXT NOT NULL DEFAULT '{}';
ALTER TABLE mcp_servers ADD COLUMN inject_params_json  TEXT NOT NULL DEFAULT '{}';

-- plans: cached plancompile output (must match planstore + planstore/store queries)
ALTER TABLE plans ADD COLUMN compiled_chain_json TEXT;
ALTER TABLE plans ADD COLUMN compiled_chain_id VARCHAR(255);
ALTER TABLE plans ADD COLUMN compile_executor_chain_id VARCHAR(255);

ALTER TABLE terminal_sessions ADD COLUMN workspace_id VARCHAR(255);

