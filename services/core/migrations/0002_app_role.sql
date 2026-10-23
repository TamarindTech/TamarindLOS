-- Tamarind LOS: application database role
--
-- The service connects as tamarind_app, which is NOT the table owner and NOT a
-- superuser, so row-level security always applies. The password is set separately
-- by `tamarind migrate` from TAMARIND_APP_DB_PASSWORD.

BEGIN;

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'tamarind_app') THEN
    CREATE ROLE tamarind_app LOGIN;
  END IF;
END;
$$;

GRANT USAGE ON SCHEMA tamarind TO tamarind_app;
GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA tamarind TO tamarind_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA tamarind TO tamarind_app;

-- Audit records are insert-only for the application, in addition to the triggers.
REVOKE UPDATE ON tamarind.events, tamarind.decision_records, tamarind.tool_invocations FROM tamarind_app;
-- Tenants and products are managed by administrators, not by tool calls.
REVOKE INSERT, UPDATE ON tamarind.tenants, tamarind.products FROM tamarind_app;

ALTER ROLE tamarind_app SET search_path = tamarind, public;

COMMIT;
