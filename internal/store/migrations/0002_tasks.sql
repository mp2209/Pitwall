-- Phase 3: task traces. A task is one agent run, from its first model call to
-- its last. Calls join a task by the x-pitwall-task header or, for agents that
-- can't send it, by inference from the conversation they resend.

CREATE TABLE tasks (
    id                    text PRIMARY KEY,
    key_id                bigint NOT NULL REFERENCES api_keys (id),
    team_id               bigint REFERENCES teams (id),
    source                text NOT NULL CHECK (source IN ('header', 'inferred')),
    -- The x-pitwall-task value the client sent (header tasks only).
    external_id           text,
    agent                 text NOT NULL DEFAULT '',
    -- Free-form label for the harness setup under test (x-pitwall-config),
    -- for example "sonnet/cache-on/trim".
    config_label          text NOT NULL DEFAULT '',
    started_at            timestamptz NOT NULL,
    ended_at              timestamptz NOT NULL,
    calls                 integer NOT NULL DEFAULT 0,
    failed_calls          integer NOT NULL DEFAULT 0,
    input_tokens          bigint NOT NULL DEFAULT 0, -- uncached
    cached_read_tokens    bigint NOT NULL DEFAULT 0,
    cache_write_tokens    bigint NOT NULL DEFAULT 0, -- both lifetimes
    output_tokens         bigint NOT NULL DEFAULT 0,
    prompt_tokens         bigint NOT NULL DEFAULT 0, -- all input, summed over calls
    repeated_input_tokens bigint NOT NULL DEFAULT 0,
    max_context_tokens    bigint NOT NULL DEFAULT 0, -- largest prompt of any call
    cost_usd              numeric(18, 9) NOT NULL DEFAULT 0,
    estimated             boolean NOT NULL DEFAULT false,
    outcome               text NOT NULL DEFAULT 'unknown'
                          CHECK (outcome IN ('solved', 'failed', 'stopped', 'unknown')),
    guard_events          integer NOT NULL DEFAULT 0,
    created_at            timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX tasks_config_outcome ON tasks (config_label, outcome);
CREATE INDEX tasks_key_started ON tasks (key_id, started_at);

-- Every call also gets the task inference would have given it, even when the
-- header named one, so inference accuracy can be measured against the header.
ALTER TABLE requests ADD COLUMN inferred_task_id text;
CREATE INDEX requests_inferred_task_id ON requests (inferred_task_id) WHERE inferred_task_id IS NOT NULL;
