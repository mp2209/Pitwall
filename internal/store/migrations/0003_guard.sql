-- Phase 4: runaway-agent guard events per call. guard_signal lists the
-- signals that fired as "signal:action" (comma-separated); guard_action is
-- the strongest action taken on the call ('warn' or 'stop').
ALTER TABLE requests ADD COLUMN guard_signal text NOT NULL DEFAULT '';
ALTER TABLE requests ADD COLUMN guard_action text NOT NULL DEFAULT '';
