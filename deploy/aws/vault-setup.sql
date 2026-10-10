BEGIN;
CREATE SCHEMA IF NOT EXISTS relayops_private;
REVOKE ALL ON SCHEMA relayops_private FROM PUBLIC, anon, authenticated;
CREATE TABLE IF NOT EXISTS relayops_private.runtime_settings (
  name text PRIMARY KEY,
  value text NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE relayops_private.runtime_settings ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON relayops_private.runtime_settings FROM PUBLIC, anon, authenticated;
GRANT USAGE ON SCHEMA vault TO postgres;
GRANT SELECT ON vault.secrets, vault.decrypted_secrets TO postgres;
GRANT EXECUTE ON FUNCTION vault.create_secret(text,text,text,uuid), vault.update_secret(uuid,text,text,text,uuid) TO postgres;
REVOKE ALL ON SCHEMA vault FROM PUBLIC, anon, authenticated;
REVOKE ALL ON vault.secrets, vault.decrypted_secrets FROM PUBLIC, anon, authenticated;
REVOKE EXECUTE ON FUNCTION vault.create_secret(text,text,text,uuid), vault.update_secret(uuid,text,text,text,uuid) FROM PUBLIC, anon, authenticated;
COMMIT;
