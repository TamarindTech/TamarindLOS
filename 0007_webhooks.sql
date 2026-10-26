-- Tamarind LOS: outbound webhooks (outbox relay)

BEGIN;
SET search_path = tamarind, public;

-- Where a tenant wants its events delivered. The secret signs each delivery
-- (HMAC-SHA256 over "<timestamp>.<body>"). It is shown once at subscription.
CREATE TABLE webhook_subscriptions (
  id            uuid NOT NULL DEFAULT gen_random_uuid(),
  tenant_id     uuid NOT NULL REFERENCES tenants (id),
  url           text NOT NULL,
  description   text,
  event_types   text[] NOT NULL DEFAULT '{}',   -- empty: every event
  secret        text NOT NULL,
  enabled       boolean NOT NULL DEFAULT true,
  created_by    text NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  removed_at    timestamptz,
  PRIMARY KEY (id),
  UNIQUE (tenant_id, id)
);

-- One row per event per subscription. The relay fans outbox rows out into deliveries
-- and a sender posts them with backoff.
CREATE TABLE webhook_deliveries (
  id               uuid NOT NULL DEFAULT gen_random_uuid(),
  tenant_id        uuid NOT NULL REFERENCES tenants (id),
  subscription_id  uuid NOT NULL,
  event_id         uuid NOT NULL REFERENCES events (id),
  status           text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'delivered', 'failed')),
  attempts         integer NOT NULL DEFAULT 0,
  next_attempt_at  timestamptz NOT NULL DEFAULT now(),
  last_status      integer,
  last_error       text,
  created_at       timestamptz NOT NULL DEFAULT now(),
  delivered_at     timestamptz,
  PRIMARY KEY (id),
  UNIQUE (subscription_id, event_id),
  FOREIGN KEY (tenant_id, subscription_id) REFERENCES webhook_subscriptions (tenant_id, id)
);
CREATE INDEX webhook_deliveries_due_idx ON webhook_deliveries (next_attempt_at) WHERE status = 'pending';

DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['webhook_subscriptions', 'webhook_deliveries'] LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON %I
         USING (tenant_id = current_setting(''tamarind.tenant_id'', true)::uuid)
         WITH CHECK (tenant_id = current_setting(''tamarind.tenant_id'', true)::uuid)', t);
  END LOOP;
END;
$$;

GRANT SELECT, INSERT, UPDATE ON webhook_subscriptions, webhook_deliveries TO tamarind_app;

COMMIT;
