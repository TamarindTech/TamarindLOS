-- Tamarind LOS: one open run per agent even when the run has no application

BEGIN;
SET search_path = tamarind, public;

-- NULL application ids were distinct under the old index, so an agent could open any
-- number of application-less runs at once.
DROP INDEX agent_runs_one_active_idx;
CREATE UNIQUE INDEX agent_runs_one_active_idx ON agent_runs (tenant_id, agent, application_id) NULLS NOT DISTINCT
  WHERE status = 'running';

COMMIT;
