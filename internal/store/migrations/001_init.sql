-- RelayOps APIM: initial schema
-- Config tables fire pg_notify('relayops_config') so every gateway node
-- hot-reloads within milliseconds of any change (admin API, psql, GitOps...).

CREATE TABLE plans (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name                  TEXT NOT NULL UNIQUE,
    description           TEXT NOT NULL DEFAULT '',
    rate_limit_per_minute INT  NOT NULL DEFAULT 60 CHECK (rate_limit_per_minute >= 0), -- 0 = unlimited
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE apis (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name                  TEXT NOT NULL UNIQUE,
    description           TEXT NOT NULL DEFAULT '',
    base_path             TEXT NOT NULL UNIQUE CHECK (base_path LIKE '/%'),
    upstream_url          TEXT NOT NULL,
    strip_path            BOOLEAN NOT NULL DEFAULT true,
    auth_type             TEXT NOT NULL DEFAULT 'none' CHECK (auth_type IN ('none', 'api_key', 'jwt')),
    jwt_secret            TEXT NOT NULL DEFAULT '',
    rate_limit_per_minute INT  NOT NULL DEFAULT 0 CHECK (rate_limit_per_minute >= 0), -- per client, 0 = unlimited
    timeout_ms            INT  NOT NULL DEFAULT 30000 CHECK (timeout_ms > 0),
    cors_enabled          BOOLEAN NOT NULL DEFAULT false,
    request_headers       JSONB NOT NULL DEFAULT '{}'::jsonb,
    enabled               BOOLEAN NOT NULL DEFAULT true,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE consumers (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name       TEXT NOT NULL UNIQUE,
    email      TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE api_keys (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    consumer_id UUID NOT NULL REFERENCES consumers(id) ON DELETE CASCADE,
    name        TEXT NOT NULL DEFAULT 'default',
    key_prefix  TEXT NOT NULL,             -- first chars, for display only
    key_hash    TEXT NOT NULL UNIQUE,      -- sha256(key) hex; raw key is never stored
    active      BOOLEAN NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX api_keys_consumer_idx ON api_keys (consumer_id);

CREATE TABLE subscriptions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    consumer_id UUID NOT NULL REFERENCES consumers(id) ON DELETE CASCADE,
    api_id      UUID NOT NULL REFERENCES apis(id) ON DELETE CASCADE,
    plan_id     UUID REFERENCES plans(id) ON DELETE SET NULL,
    active      BOOLEAN NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (consumer_id, api_id)
);

CREATE TABLE request_logs (
    id            BIGSERIAL PRIMARY KEY,
    ts            TIMESTAMPTZ NOT NULL,
    node_id       TEXT NOT NULL DEFAULT '',
    request_id    TEXT NOT NULL DEFAULT '',
    api_id        UUID,
    api_name      TEXT NOT NULL DEFAULT '',
    consumer_id   UUID,
    consumer_name TEXT NOT NULL DEFAULT '',
    method        TEXT NOT NULL,
    path          TEXT NOT NULL,
    status        INT  NOT NULL,
    latency_ms    DOUBLE PRECISION NOT NULL,
    bytes_out     BIGINT NOT NULL DEFAULT 0,
    client_ip     TEXT NOT NULL DEFAULT '',
    error         TEXT NOT NULL DEFAULT ''
);
CREATE INDEX request_logs_ts_idx     ON request_logs (ts DESC);
CREATE INDEX request_logs_api_ts_idx ON request_logs (api_id, ts DESC);

-- Real-time config propagation -------------------------------------------
CREATE OR REPLACE FUNCTION relayops_notify_config() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('relayops_config',
        json_build_object('table', TG_TABLE_NAME, 'op', TG_OP, 'at', now())::text);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER apis_notify          AFTER INSERT OR UPDATE OR DELETE OR TRUNCATE ON apis          FOR EACH STATEMENT EXECUTE FUNCTION relayops_notify_config();
CREATE TRIGGER plans_notify         AFTER INSERT OR UPDATE OR DELETE OR TRUNCATE ON plans         FOR EACH STATEMENT EXECUTE FUNCTION relayops_notify_config();
CREATE TRIGGER consumers_notify     AFTER INSERT OR UPDATE OR DELETE OR TRUNCATE ON consumers     FOR EACH STATEMENT EXECUTE FUNCTION relayops_notify_config();
CREATE TRIGGER api_keys_notify      AFTER INSERT OR UPDATE OR DELETE OR TRUNCATE ON api_keys      FOR EACH STATEMENT EXECUTE FUNCTION relayops_notify_config();
CREATE TRIGGER subscriptions_notify AFTER INSERT OR UPDATE OR DELETE OR TRUNCATE ON subscriptions FOR EACH STATEMENT EXECUTE FUNCTION relayops_notify_config();

-- Default plans
INSERT INTO plans (name, description, rate_limit_per_minute) VALUES
    ('Free',      'Evaluation tier',          60),
    ('Pro',       'Production tier',          1000),
    ('Unlimited', 'Internal / trusted apps',  0);
