-- Tamarind LOS: agent runs and the shared work queue (Phase 0)

BEGIN;
SET search_path = tamarind, public;

-- Every tool call an agent makes inside a run is linked to it, so a run can be
-- replayed step by step from the invocation log.
ALTER TABLE tool_invocations ADD COLUMN agent_run_id uuid;
CREATE INDEX tool_invocations_run_idx ON tool_invocations (agent_run_id, started_at) WHERE agent_run_id IS NOT NULL;

-- What woke the agent, and the task it handed to a person when it escalated.
ALTER TABLE agent_runs ADD COLUMN trigger_event_type text;
ALTER TABLE agent_runs ADD COLUMN escalation_task_id uuid;
CREATE INDEX agent_runs_application_idx ON agent_runs (tenant_id, application_id, started_at);

-- Tasks an agent creates during a run point back to it.
ALTER TABLE tasks ADD COLUMN agent_run_id uuid;
CREATE INDEX tasks_application_idx ON tasks (tenant_id, application_id, status);

COMMIT;
