-- Tamarind LOS: initial schema
-- Target: PostgreSQL 16+
-- Spec: docs/specs/04-data-model.md
--
-- Conventions
--   * Every business table carries tenant_id and is protected by row-level security.
--   * Primary keys are UUIDs (the application generates UUIDv7; gen_random_uuid() is a fallback).
--   * Money is stored as integer minor units (bigint) plus an ISO 4217 currency code.
--   * Principals are stored as typed strings: user:..., agent:..., svc:..., borrower:...
--   * events and decision_records are append-only (enforced by triggers).

BEGIN;

CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE SCHEMA IF NOT EXISTS tamarind;
SET search_path = tamarind, public;

-- ---------------------------------------------------------------------------
-- Types
-- ---------------------------------------------------------------------------
CREATE TYPE application_state AS ENUM (
  'draft', 'submitted', 'intake', 'verification', 'referred', 'underwriting',
  'decisioned', 'offered', 'accepted', 'handed_off', 'closed', 'expired', 'withdrawn'
);
CREATE TYPE party_kind      AS ENUM ('individual', 'business');
CREATE TYPE party_role      AS ENUM ('applicant', 'co_applicant', 'guarantor', 'director', 'beneficial_owner');
CREATE TYPE evidence_status AS ENUM ('extracted', 'verified', 'rejected', 'needs_review');
CREATE TYPE check_status    AS ENUM ('pending', 'passed', 'failed', 'needs_review', 'error');
CREATE TYPE task_status     AS ENUM ('open', 'in_progress', 'done', 'cancelled');
CREATE TYPE approval_status AS ENUM ('pending', 'approved', 'rejected', 'expired', 'cancelled');
CREATE TYPE offer_status    AS ENUM ('draft', 'sent', 'accepted', 'expired', 'withdrawn');
CREATE TYPE invocation_outcome AS ENUM ('ok', 'error', 'approval_required', 'denied');

-- ---------------------------------------------------------------------------
-- Tenancy and products
-- ---------------------------------------------------------------------------
CREATE TABLE tenants (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  slug        text NOT NULL UNIQUE,
  name        text NOT NULL,
  settings    jsonb NOT NULL DEFAULT '{}',
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE products (
  id                   uuid NOT NULL DEFAULT gen_random_uuid(),
  tenant_id            uuid NOT NULL REFERENCES tenants (id),
  code                 text NOT NULL,
  version              text NOT NULL,
  name                 text NOT NULL,
  status               text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'active', 'retired')),
  currency             char(3) NOT NULL,
  min_amount_minor     bigint NOT NULL CHECK (min_amount_minor > 0),
  max_amount_minor     bigint NOT NULL,
  min_term_months      integer NOT NULL CHECK (min_term_months > 0),
  max_term_months      integer NOT NULL,
  policy_pack          text NOT NULL,
  policy_pack_version  text NOT NULL,
  definition           jsonb NOT NULL,
  created_at           timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (id),
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, code, version),
  CHECK (max_amount_minor >= min_amount_minor),
  CHECK (max_term_months >= min_term_months)
);

-- ---------------------------------------------------------------------------
-- Applications and parties
-- ---------------------------------------------------------------------------
CREATE TABLE applications (
  id                      uuid NOT NULL DEFAULT gen_random_uuid(),
  tenant_id               uuid NOT NULL REFERENCES tenants (id),
  product_id              uuid NOT NULL,
  reference               text NOT NULL,
  state                   application_state NOT NULL DEFAULT 'draft',
  state_version           integer NOT NULL DEFAULT 0,
  state_machine_version   text NOT NULL,
  requested_amount_minor  bigint NOT NULL CHECK (requested_amount_minor > 0),
  currency                char(3) NOT NULL,
  requested_term_months   integer NOT NULL CHECK (requested_term_months > 0),
  purpose                 text,
  channel                 text NOT NULL DEFAULT 'web',
  completeness_pct        numeric(5,2) NOT NULL DEFAULT 0 CHECK (completeness_pct BETWEEN 0 AND 100),
  decision_outcome        text CHECK (decision_outcome IN ('approve', 'approve_with_conditions', 'decline')),
  data                    jsonb NOT NULL DEFAULT '{}',
  created_by              text NOT NULL,
  created_at              timestamptz NOT NULL DEFAULT now(),
  updated_at              timestamptz NOT NULL DEFAULT now(),
  submitted_at            timestamptz,
  closed_at               timestamptz,
  PRIMARY KEY (id),
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, reference),
  FOREIGN KEY (tenant_id, product_id) REFERENCES products (tenant_id, id)
);
CREATE INDEX applications_state_idx ON applications (tenant_id, state);

CREATE TABLE parties (
  id                    uuid NOT NULL DEFAULT gen_random_uuid(),
  tenant_id             uuid NOT NULL REFERENCES tenants (id),
  kind                  party_kind NOT NULL,
  legal_name            text NOT NULL,
  display_name          text,
  date_of_birth         date,
  registration_number   text,
  country               char(2),
  contact               jsonb NOT NULL DEFAULT '{}',
  -- Restricted identifiers (national ID, bank account numbers) are encrypted by the
  -- application with a per-tenant key before storage. Never store them in plain text.
  restricted_ciphertext bytea,
  restricted_key_id     text,
  created_at            timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (id),
  UNIQUE (tenant_id, id),
  CHECK (kind = 'individual' OR date_of_birth IS NULL)
);

CREATE TABLE application_parties (
  tenant_id       uuid NOT NULL,
  application_id  uuid NOT NULL,
  party_id        uuid NOT NULL,
  role            party_role NOT NULL,
  ownership_pct   numeric(5,2) CHECK (ownership_pct BETWEEN 0 AND 100),
  added_at        timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (application_id, party_id, role),
  FOREIGN KEY (tenant_id, application_id) REFERENCES applications (tenant_id, id),
  FOREIGN KEY (tenant_id, party_id) REFERENCES parties (tenant_id, id)
);
CREATE UNIQUE INDEX one_applicant_per_application
  ON application_parties (application_id) WHERE role = 'applicant';

CREATE TABLE consents (
  id              uuid NOT NULL DEFAULT gen_random_uuid(),
  tenant_id       uuid NOT NULL,
  party_id        uuid NOT NULL,
  application_id  uuid,
  scope           text NOT NULL CHECK (scope IN ('credit_bureau', 'bank_data', 'kyc', 'e_communication', 'data_sharing')),
  evidence        jsonb NOT NULL,
  granted_at      timestamptz NOT NULL DEFAULT now(),
  expires_at      timestamptz,
  revoked_at      timestamptz,
  recorded_by     text NOT NULL,
  PRIMARY KEY (id),
  UNIQUE (tenant_id, id),
  FOREIGN KEY (tenant_id, party_id) REFERENCES parties (tenant_id, id),
  FOREIGN KEY (tenant_id, application_id) REFERENCES applications (tenant_id, id)
);
CREATE INDEX consents_party_idx ON consents (tenant_id, party_id, scope);

-- ---------------------------------------------------------------------------
-- Documents and evidence
-- ---------------------------------------------------------------------------
CREATE TABLE documents (
  id                         uuid NOT NULL DEFAULT gen_random_uuid(),
  tenant_id                  uuid NOT NULL,
  application_id             uuid NOT NULL,
  party_id                   uuid,
  document_type              text,
  classification_confidence  numeric(4,3) CHECK (classification_confidence BETWEEN 0 AND 1),
  status                     text NOT NULL DEFAULT 'received'
                               CHECK (status IN ('received', 'classified', 'extracted', 'rejected')),
  storage_key                text NOT NULL,
  sha256                     char(64) NOT NULL,
  mime_type                  text NOT NULL,
  size_bytes                 bigint NOT NULL CHECK (size_bytes > 0),
  uploaded_by                text NOT NULL,
  created_at                 timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (id),
  UNIQUE (tenant_id, id),
  FOREIGN KEY (tenant_id, application_id) REFERENCES applications (tenant_id, id),
  FOREIGN KEY (tenant_id, party_id) REFERENCES parties (tenant_id, id)
);
CREATE INDEX documents_application_idx ON documents (application_id);

CREATE TABLE evidence (
  id                 uuid NOT NULL DEFAULT gen_random_uuid(),
  tenant_id          uuid NOT NULL,
  application_id     uuid NOT NULL,
  document_id        uuid NOT NULL,
  field              text NOT NULL,
  value              jsonb NOT NULL,
  confidence         numeric(4,3) NOT NULL CHECK (confidence BETWEEN 0 AND 1),
  page               integer,
  bbox               jsonb,
  extractor          text NOT NULL,
  extractor_version  text NOT NULL,
  status             evidence_status NOT NULL DEFAULT 'extracted',
  verified_by        text,
  created_at         timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (id),
  UNIQUE (tenant_id, id),
  FOREIGN KEY (tenant_id, application_id) REFERENCES applications (tenant_id, id),
  FOREIGN KEY (tenant_id, document_id) REFERENCES documents (tenant_id, id)
);
CREATE INDEX evidence_application_idx ON evidence (application_id, field);

-- ---------------------------------------------------------------------------
-- Checks
-- ---------------------------------------------------------------------------
CREATE TABLE checks (
  id              uuid NOT NULL DEFAULT gen_random_uuid(),
  tenant_id       uuid NOT NULL,
  application_id  uuid NOT NULL,
  party_id        uuid,
  check_type      text NOT NULL CHECK (check_type IN ('kyc', 'kyb', 'sanctions', 'credit_bureau', 'bank_analysis', 'fraud')),
  provider        text NOT NULL,
  status          check_status NOT NULL DEFAULT 'pending',
  result          jsonb NOT NULL DEFAULT '{}',
  external_ref    text,
  consent_id      uuid,
  requested_by    text NOT NULL,
  requested_at    timestamptz NOT NULL DEFAULT now(),
  completed_at    timestamptz,
  PRIMARY KEY (id),
  UNIQUE (tenant_id, id),
  FOREIGN KEY (tenant_id, application_id) REFERENCES applications (tenant_id, id),
  FOREIGN KEY (tenant_id, party_id) REFERENCES parties (tenant_id, id),
  FOREIGN KEY (tenant_id, consent_id) REFERENCES consents (tenant_id, id)
);
CREATE INDEX checks_application_idx ON checks (application_id, check_type);

-- ---------------------------------------------------------------------------
-- Decisions, offers, work
-- ---------------------------------------------------------------------------
CREATE TABLE decision_records (
  id              uuid NOT NULL DEFAULT gen_random_uuid(),
  tenant_id       uuid NOT NULL,
  application_id  uuid NOT NULL,
  kind            text NOT NULL CHECK (kind IN ('recommendation', 'decision')),
  outcome         text NOT NULL CHECK (outcome IN ('approve', 'approve_with_conditions', 'decline', 'refer')),
  terms           jsonb,
  conditions      jsonb NOT NULL DEFAULT '[]',
  reason_codes    text[] NOT NULL DEFAULT '{}',
  inputs          jsonb NOT NULL DEFAULT '{}',
  policy          jsonb NOT NULL,
  models          jsonb NOT NULL DEFAULT '[]',
  narrative       text,
  confidence      numeric(4,3) CHECK (confidence BETWEEN 0 AND 1),
  actor           text NOT NULL,
  on_behalf_of    text,
  based_on        uuid,
  created_at      timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (id),
  UNIQUE (tenant_id, id),
  FOREIGN KEY (tenant_id, application_id) REFERENCES applications (tenant_id, id),
  FOREIGN KEY (tenant_id, based_on) REFERENCES decision_records (tenant_id, id),
  -- A final decision must be made by a human. Approvals must reference the recommendation they act on.
  -- (Tenants that enable L4 autonomy for a segment must change this constraint through a reviewed migration.)
  CHECK (kind = 'recommendation' OR (actor LIKE 'user:%' AND (based_on IS NOT NULL OR outcome = 'decline')))
);
CREATE INDEX decision_records_application_idx ON decision_records (application_id, created_at);

CREATE TABLE offers (
  id                  uuid NOT NULL DEFAULT gen_random_uuid(),
  tenant_id           uuid NOT NULL,
  application_id      uuid NOT NULL,
  decision_record_id  uuid NOT NULL,
  status              offer_status NOT NULL DEFAULT 'draft',
  amount_minor        bigint NOT NULL CHECK (amount_minor > 0),
  currency            char(3) NOT NULL,
  term_months         integer NOT NULL CHECK (term_months > 0),
  rate_apr_bps        integer NOT NULL CHECK (rate_apr_bps >= 0),
  fees                jsonb NOT NULL DEFAULT '[]',
  schedule_summary    jsonb NOT NULL,
  disclosures         jsonb NOT NULL DEFAULT '{}',
  expires_at          timestamptz NOT NULL,
  sent_at             timestamptz,
  accepted_at         timestamptz,
  acceptance_evidence jsonb,
  created_at          timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (id),
  UNIQUE (tenant_id, id),
  FOREIGN KEY (tenant_id, application_id) REFERENCES applications (tenant_id, id),
  FOREIGN KEY (tenant_id, decision_record_id) REFERENCES decision_records (tenant_id, id)
);

CREATE TABLE tasks (
  id              uuid NOT NULL DEFAULT gen_random_uuid(),
  tenant_id       uuid NOT NULL,
  application_id  uuid,
  kind            text NOT NULL,
  title           text NOT NULL,
  details         jsonb NOT NULL DEFAULT '{}',
  assignee        text NOT NULL,
  status          task_status NOT NULL DEFAULT 'open',
  due_at          timestamptz,
  created_by      text NOT NULL,
  completed_by    text,
  outcome         jsonb,
  created_at      timestamptz NOT NULL DEFAULT now(),
  completed_at    timestamptz,
  PRIMARY KEY (id),
  UNIQUE (tenant_id, id),
  FOREIGN KEY (tenant_id, application_id) REFERENCES applications (tenant_id, id)
);
CREATE INDEX tasks_queue_idx ON tasks (tenant_id, assignee, status);

CREATE TABLE approval_requests (
  id              uuid NOT NULL DEFAULT gen_random_uuid(),
  tenant_id       uuid NOT NULL,
  application_id  uuid,
  tool            text NOT NULL,
  tool_input      jsonb NOT NULL,
  requested_by    text NOT NULL,
  on_behalf_of    text,
  required_role   text NOT NULL,
  reason          text NOT NULL,
  status          approval_status NOT NULL DEFAULT 'pending',
  decided_by      text,
  decided_at      timestamptz,
  decision_note   text,
  expires_at      timestamptz,
  created_at      timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (id),
  UNIQUE (tenant_id, id),
  FOREIGN KEY (tenant_id, application_id) REFERENCES applications (tenant_id, id),
  -- Maker-checker: the requester, and whoever it acted for, cannot decide the request.
  CHECK (decided_by IS NULL OR (decided_by <> requested_by AND decided_by IS DISTINCT FROM on_behalf_of))
);
CREATE INDEX approval_requests_pending_idx ON approval_requests (tenant_id, required_role) WHERE status = 'pending';

-- ---------------------------------------------------------------------------
-- Gateway, agents, events and audit
-- ---------------------------------------------------------------------------
CREATE TABLE tool_invocations (
  id               uuid NOT NULL DEFAULT gen_random_uuid(),
  tenant_id        uuid NOT NULL REFERENCES tenants (id),
  tool             text NOT NULL,
  tool_version     text NOT NULL,
  actor            text NOT NULL,
  on_behalf_of     text,
  application_id   uuid,
  idempotency_key  text NOT NULL,
  input_sha256     char(64) NOT NULL,
  reason           text,
  dry_run          boolean NOT NULL DEFAULT false,
  autonomy         text NOT NULL,
  policy_result    jsonb NOT NULL,
  outcome          invocation_outcome NOT NULL,
  error_code       text,
  response         jsonb,
  trace_id         text,
  started_at       timestamptz NOT NULL DEFAULT now(),
  finished_at      timestamptz,
  PRIMARY KEY (id),
  UNIQUE (tenant_id, actor, tool, idempotency_key),
  CHECK (actor NOT LIKE 'agent:%' OR reason IS NOT NULL)
);
CREATE INDEX tool_invocations_application_idx ON tool_invocations (application_id, started_at);

CREATE TABLE agent_runs (
  id                uuid NOT NULL DEFAULT gen_random_uuid(),
  tenant_id         uuid NOT NULL REFERENCES tenants (id),
  agent             text NOT NULL,
  agent_version     text NOT NULL,
  application_id    uuid,
  trigger_event_id  uuid,
  status            text NOT NULL DEFAULT 'running'
                      CHECK (status IN ('running', 'succeeded', 'escalated', 'failed', 'cancelled')),
  steps             integer NOT NULL DEFAULT 0,
  tokens_in         bigint NOT NULL DEFAULT 0,
  tokens_out        bigint NOT NULL DEFAULT 0,
  cost_micro_usd    bigint NOT NULL DEFAULT 0,
  summary           text,
  started_at        timestamptz NOT NULL DEFAULT now(),
  finished_at       timestamptz,
  PRIMARY KEY (id)
);
-- At most one active run per agent per application.
CREATE UNIQUE INDEX agent_runs_one_active_idx ON agent_runs (tenant_id, agent, application_id) WHERE status = 'running';

CREATE TABLE events (
  seq                bigint GENERATED ALWAYS AS IDENTITY,
  id                 uuid NOT NULL DEFAULT gen_random_uuid(),
  tenant_id          uuid NOT NULL REFERENCES tenants (id),
  aggregate_type     text NOT NULL,
  aggregate_id       uuid NOT NULL,
  aggregate_version  integer NOT NULL,
  type               text NOT NULL,
  actor              text NOT NULL,
  on_behalf_of       text,
  occurred_at        timestamptz NOT NULL DEFAULT now(),
  trace_id           text,
  data               jsonb NOT NULL DEFAULT '{}',
  -- Per-tenant hash chain: hash = sha256(prev_hash || canonical_json(event without hash)).
  prev_hash          bytea,
  hash               bytea NOT NULL,
  PRIMARY KEY (id),
  UNIQUE (seq),
  UNIQUE (aggregate_type, aggregate_id, aggregate_version)
);
CREATE INDEX events_tenant_seq_idx ON events (tenant_id, seq);

CREATE TABLE outbox (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  event_id      uuid NOT NULL REFERENCES events (id),
  tenant_id     uuid NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  published_at  timestamptz,
  attempts      integer NOT NULL DEFAULT 0,
  last_error    text
);
CREATE INDEX outbox_unpublished_idx ON outbox (id) WHERE published_at IS NULL;

-- ---------------------------------------------------------------------------
-- Append-only enforcement
-- ---------------------------------------------------------------------------
CREATE FUNCTION forbid_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'table % is append-only', TG_TABLE_NAME;
END;
$$;

CREATE TRIGGER events_append_only
  BEFORE UPDATE OR DELETE ON events
  FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TRIGGER decision_records_append_only
  BEFORE UPDATE OR DELETE ON decision_records
  FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ---------------------------------------------------------------------------
-- Row-level security: every tenant-scoped table is filtered by the session's tenant.
-- The application sets it per transaction:  SET LOCAL tamarind.tenant_id = '<uuid>';
-- ---------------------------------------------------------------------------
DO $$
DECLARE
  t text;
BEGIN
  FOREACH t IN ARRAY ARRAY[
    'products', 'applications', 'parties', 'application_parties', 'consents',
    'documents', 'evidence', 'checks', 'decision_records', 'offers', 'tasks',
    'approval_requests', 'tool_invocations', 'agent_runs', 'events', 'outbox'
  ]
  LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON %I
         USING (tenant_id = current_setting(''tamarind.tenant_id'', true)::uuid)
         WITH CHECK (tenant_id = current_setting(''tamarind.tenant_id'', true)::uuid)', t);
  END LOOP;
END;
$$;

COMMIT;
