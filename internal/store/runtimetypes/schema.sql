
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
ALTER TABLE kv ADD COLUMN IF NOT EXISTS workspace_id VARCHAR(255) NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS idx_kv_key_workspace ON kv(key, workspace_id);

CREATE TABLE IF NOT EXISTS remote_tools (
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

ALTER TABLE remote_tools ADD COLUMN IF NOT EXISTS body_properties BYTEA;
ALTER TABLE remote_tools ADD COLUMN IF NOT EXISTS headers JSONB;
ALTER TABLE remote_tools ADD COLUMN IF NOT EXISTS inject_params_json JSONB DEFAULT '{}';
ALTER TABLE remote_tools ADD COLUMN IF NOT EXISTS spec_url TEXT;
ALTER TABLE remote_tools ADD COLUMN IF NOT EXISTS auth_flow_json JSONB;
ALTER TABLE remote_tools ADD COLUMN IF NOT EXISTS insecure_skip_verify BOOLEAN NOT NULL DEFAULT FALSE;



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

-- message_indices: agent_id reserved for future session -> agent attribution
-- (external ACP / chain agents driving a session). Nullable; not wired to any
-- code path yet.
ALTER TABLE message_indices ADD COLUMN IF NOT EXISTS agent_id VARCHAR(255);

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
ALTER TABLE mcp_servers ADD COLUMN IF NOT EXISTS oauth_client_id TEXT NOT NULL DEFAULT '';
ALTER TABLE mcp_servers ADD COLUMN IF NOT EXISTS oauth_client_secret_env TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_mcp_servers_created_at ON mcp_servers(created_at);

-- agents: polymorphic declared-agent resource. `kind` selects which
-- kind-specific shape `config_json` holds ('external_acp': somebody else's
-- ACP program; 'chain': one of the runtime's own task chains, run as a unit).
-- Config lives in the typed JSON column rather than flat per-kind columns (contrast
-- mcp_servers, which can use flat columns because it has only one kind) so
-- adding a new kind never requires a schema migration.
-- harness_id is a reserved FK seam (no harness table/service exists yet in
-- this slice; NULL means "the implicit serve harness"). workspace_id follows
-- the same scoping convention as kv/message_indices.
-- source/registry_id/registry_version are system-managed provenance for
-- display and updates (e.g. "seeded from the ACP registry"): source is
-- 'registry' or 'manual', registry_id/registry_version record the catalog
-- entry an agent was seeded from. They are kept OUT of config_json — the
-- user-editable run spec — so `contenox agent edit` never touches them.
CREATE TABLE IF NOT EXISTS agents (
    id               VARCHAR(255) PRIMARY KEY,
    name             VARCHAR(255) NOT NULL UNIQUE,
    kind             VARCHAR(50)  NOT NULL,
    enabled          BOOLEAN NOT NULL DEFAULT true,
    config_json      JSONB NOT NULL DEFAULT '{}',
    harness_id       VARCHAR(255),
    workspace_id     VARCHAR(255),
    source           VARCHAR(50),
    registry_id      VARCHAR(255),
    registry_version VARCHAR(50),
    created_at       TIMESTAMP NOT NULL,
    updated_at       TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_agents_created_at ON agents(created_at);
CREATE INDEX IF NOT EXISTS idx_agents_kind ON agents(kind);

-- hitl_approvals: durable pending-and-resolved human-in-the-loop approval
-- asks (runtime/hitlservice). RequestApproval writes the row here BEFORE
-- publishing the approval_requested TaskEvent, so a `contenox serve` restart
-- mid-ask still shows it pending instead of losing it outright — the
-- previous implementation held every ask in an in-process map only (see
-- docs/development/blueprints/acp/fleet-consolidation.md, slice C1, defect
-- D3). state starts 'pending' and ends exactly once, at 'approved'/'denied'
-- (via Respond) or 'expired' (the sweeper, once expires_at passes with
-- nobody having answered, applying on_timeout — default deny).
-- diff is nullable: most tool calls have none. policy_name/matched_rule
-- mirror hitlservice.EvaluationResult so an operator can always name which
-- rule gated a given action (matched_rule NULL means the policy's
-- default_action applied, not a named rule).
-- resolution is deliberately JSONB, not BOOLEAN: today Respond only ever
-- writes an approve/deny answer, but a permission ask is answered yes/no
-- while a later ask kind (e.g. mission-mode "ask for attention") answers
-- with data instead ("which of these three?", "what value should I use?").
-- Narrowing this column to a boolean now would force a migration the moment
-- that lands. state (below) stays the queryable lifecycle fact the sweeper
-- and any inbox filter on; resolution is just the payload beside it.
CREATE TABLE IF NOT EXISTS hitl_approvals (
    id           VARCHAR(255) PRIMARY KEY,
    tools_name   VARCHAR(255) NOT NULL,
    tool_name    VARCHAR(255) NOT NULL,
    args_summary TEXT NOT NULL DEFAULT '',
    diff         TEXT,
    policy_name  VARCHAR(255) NOT NULL DEFAULT '',
    matched_rule INT,
    on_timeout   VARCHAR(20) NOT NULL DEFAULT 'deny',
    state        VARCHAR(20) NOT NULL DEFAULT 'pending',
    resolution   JSONB,
    instance_id  VARCHAR(255) NOT NULL DEFAULT '',
    session_id   VARCHAR(255) NOT NULL DEFAULT '',
    agent_name   VARCHAR(255) NOT NULL DEFAULT '',
    mission_id   VARCHAR(255),
    created_at   TIMESTAMP NOT NULL,
    expires_at   TIMESTAMP NOT NULL,
    resolved_at  TIMESTAMP
);
-- Attribution columns added after the table shipped: which UNIT is asking, not
-- just which tool it called. An inbox that can only say "write_file" is
-- unanswerable once more than one unit is running, and it breaks the invariant
-- that an operator can always name what gated an action (see
-- docs/development/blueprints/acp/fleet-consolidation.md, slice M5 and C2's
-- report). instance_id/session_id/agent_name default to '' because an ask
-- raised by a native chain turn has no fleet unit behind it; mission_id is
-- NULLABLE because not every ask has a mission (a non-mission unattended
-- session, an API caller) and '' would be indistinguishable from one.
ALTER TABLE hitl_approvals ADD COLUMN IF NOT EXISTS instance_id VARCHAR(255) NOT NULL DEFAULT '';
ALTER TABLE hitl_approvals ADD COLUMN IF NOT EXISTS session_id  VARCHAR(255) NOT NULL DEFAULT '';
ALTER TABLE hitl_approvals ADD COLUMN IF NOT EXISTS agent_name  VARCHAR(255) NOT NULL DEFAULT '';
ALTER TABLE hitl_approvals ADD COLUMN IF NOT EXISTS mission_id  VARCHAR(255);
CREATE INDEX IF NOT EXISTS idx_hitl_approvals_state_created ON hitl_approvals(state, created_at);
CREATE INDEX IF NOT EXISTS idx_hitl_approvals_state_expires ON hitl_approvals(state, expires_at);

-- plan-review feature removed: drop orphaned tables on upgraded databases.
DROP TABLE IF EXISTS plan_steps CASCADE;
DROP TABLE IF EXISTS plans CASCADE;

CREATE TABLE IF NOT EXISTS llm_model_registry (
    id          VARCHAR(255) PRIMARY KEY,
    name        VARCHAR(512) NOT NULL UNIQUE,
    source_url  VARCHAR(1024) NOT NULL,
    size_bytes  BIGINT NOT NULL DEFAULT 0,
    created_at  TIMESTAMP NOT NULL,
    updated_at  TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_llm_model_registry_created_at ON llm_model_registry(created_at);

CREATE TABLE IF NOT EXISTS local_fs_reads (
    session_id    VARCHAR(255) NOT NULL DEFAULT '',
    path          TEXT NOT NULL,
    last_read_at  TIMESTAMP NOT NULL,
    PRIMARY KEY (session_id, path)
);
