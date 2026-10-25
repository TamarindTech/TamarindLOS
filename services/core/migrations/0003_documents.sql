-- Tamarind LOS: document uploads and evidence corrections (document pipeline, Phase 0)

BEGIN;
SET search_path = tamarind, public;

-- One row per issued upload link. The link carries a random token; only its SHA-256
-- is stored. A file is stored once, scanned, then attached to the application.
CREATE TABLE uploads (
  id              uuid NOT NULL DEFAULT gen_random_uuid(),
  tenant_id       uuid NOT NULL REFERENCES tenants (id),
  application_id  uuid NOT NULL,
  party_id        uuid,
  token_sha256    char(64) NOT NULL UNIQUE,
  file_name       text NOT NULL,
  mime_type       text NOT NULL,
  max_bytes       bigint NOT NULL CHECK (max_bytes > 0),
  status          text NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'uploaded', 'rejected', 'attached')),
  reject_reason   text,
  storage_key     text,
  sha256          char(64),
  size_bytes      bigint,
  created_by      text NOT NULL,
  created_at      timestamptz NOT NULL DEFAULT now(),
  expires_at      timestamptz NOT NULL,
  uploaded_at     timestamptz,
  document_id     uuid,
  PRIMARY KEY (id),
  FOREIGN KEY (tenant_id, application_id) REFERENCES applications (tenant_id, id),
  FOREIGN KEY (tenant_id, party_id) REFERENCES parties (tenant_id, id)
);

ALTER TABLE uploads ENABLE ROW LEVEL SECURITY;
ALTER TABLE uploads FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON uploads
  USING (tenant_id = current_setting('tamarind.tenant_id', true)::uuid)
  WITH CHECK (tenant_id = current_setting('tamarind.tenant_id', true)::uuid);

-- What the uploader said the document is (classification decides document_type).
ALTER TABLE documents ADD COLUMN declared_type text;
ALTER TABLE documents ADD COLUMN file_name text;

-- When a person corrects an extracted value, the original is kept.
ALTER TABLE evidence ADD COLUMN original_value jsonb;
ALTER TABLE evidence ADD COLUMN note text;
ALTER TABLE evidence ADD COLUMN verified_at timestamptz;

GRANT SELECT, INSERT, UPDATE ON uploads TO tamarind_app;

COMMIT;
