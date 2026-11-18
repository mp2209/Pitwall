-- Phase 2: teams, users, virtual keys, prices and the per-call request log.
-- Money is USD. A budget of 0 means no limit.

CREATE TABLE teams (
    id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name               text NOT NULL UNIQUE,
    monthly_budget_usd numeric(14, 6) NOT NULL DEFAULT 0 CHECK (monthly_budget_usd >= 0),
    created_at         timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE users (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email      text NOT NULL UNIQUE,
    team_id    bigint REFERENCES teams (id),
    role       text NOT NULL DEFAULT 'dev' CHECK (role IN ('dev', 'admin')),
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Only a SHA-256 of each key is stored; key_prefix ("pw-" + 8 characters)
-- is kept to tell keys apart in listings and logs.
CREATE TABLE api_keys (
    id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id            bigint NOT NULL REFERENCES users (id),
    name               text NOT NULL DEFAULT '',
    key_hash           bytea NOT NULL UNIQUE,
    key_prefix         text NOT NULL,
    allowed_models     text[] NOT NULL DEFAULT '{}',
    monthly_budget_usd numeric(14, 6) NOT NULL DEFAULT 0 CHECK (monthly_budget_usd >= 0),
    cache_optimizer_on boolean NOT NULL DEFAULT false,
    created_at         timestamptz NOT NULL DEFAULT now(),
    revoked_at         timestamptz
);

-- Price history, USD per million tokens. A price change is a new row with a
-- later effective_from, so recorded costs never change.
CREATE TABLE model_prices (
    model                   text NOT NULL,
    speed                   text NOT NULL DEFAULT '' CHECK (speed IN ('', 'fast')),
    effective_from          timestamptz NOT NULL,
    input_per_mtok          numeric(12, 6) NOT NULL DEFAULT 0,
    cache_write_per_mtok    numeric(12, 6) NOT NULL DEFAULT 0,
    cache_write_1h_per_mtok numeric(12, 6) NOT NULL DEFAULT 0,
    cached_read_per_mtok    numeric(12, 6) NOT NULL DEFAULT 0,
    output_per_mtok         numeric(12, 6) NOT NULL DEFAULT 0,
    -- Prompts over the threshold are billed at long_context rates
    -- ({"input": ..., "cached_read": ..., ...}), for example Haiku 5.5 over 100k.
    long_context_threshold  bigint NOT NULL DEFAULT 0,
    long_context            jsonb,
    -- Multiplier for US-only inference; 0 means not applicable.
    us_multiplier           numeric(6, 4) NOT NULL DEFAULT 0,
    created_at              timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (model, speed, effective_from)
);

-- One row per model call. task_id is filled in Phase 3; repeated_input_tokens
-- and breakpoints_added in Phases 3 and 4.
CREATE TABLE requests (
    id                    text PRIMARY KEY,
    ts                    timestamptz NOT NULL,
    task_id               text,
    key_id                bigint NOT NULL REFERENCES api_keys (id),
    team_id               bigint REFERENCES teams (id),
    client_format         text NOT NULL,
    model                 text NOT NULL,
    upstream_model        text NOT NULL,
    provider              text NOT NULL,
    stream                boolean NOT NULL,
    status                integer NOT NULL,
    input_tokens          bigint NOT NULL DEFAULT 0,
    cached_read_tokens    bigint NOT NULL DEFAULT 0,
    cache_write_tokens    bigint NOT NULL DEFAULT 0,
    cache_write_1h_tokens bigint NOT NULL DEFAULT 0,
    output_tokens         bigint NOT NULL DEFAULT 0,
    repeated_input_tokens bigint NOT NULL DEFAULT 0,
    breakpoints_added     integer NOT NULL DEFAULT 0,
    speed                 text NOT NULL DEFAULT '',
    inference_geo         text NOT NULL DEFAULT '',
    estimated             boolean NOT NULL DEFAULT false,
    priced                boolean NOT NULL DEFAULT false,
    cost_usd              numeric(18, 9) NOT NULL DEFAULT 0,
    reserved_usd          numeric(18, 9) NOT NULL DEFAULT 0,
    latency_ms            integer NOT NULL DEFAULT 0,
    ttft_ms               integer,
    overhead_ms           real NOT NULL DEFAULT 0,
    client_gone           boolean NOT NULL DEFAULT false,
    error                 text NOT NULL DEFAULT ''
);

CREATE INDEX requests_task_id ON requests (task_id) WHERE task_id IS NOT NULL;
CREATE INDEX requests_key_ts ON requests (key_id, ts);
CREATE INDEX requests_team_ts ON requests (team_id, ts) WHERE team_id IS NOT NULL;
