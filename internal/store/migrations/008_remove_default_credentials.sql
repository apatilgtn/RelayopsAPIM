-- 008_remove_default_credentials.sql
-- Earlier versions gave every account created without a password (including
-- SSO-provisioned administrators) the shared password "admin123", and seeded
-- admin@relayops.local with the bearer token / password "relayops-admin" that
-- kept working after RELAYOPS_ADMIN_TOKEN was changed. Disable both.
-- Affected accounts sign in through SSO or the cluster token until an
-- administrator sets a password for them.

-- Sessions that may have been obtained with those credentials end now.
DELETE FROM admin_sessions s
 USING admin_users u
 WHERE s.user_id = u.id
   AND (u.password_hash = encode(sha256('admin123'::bytea), 'hex')
        OR u.token_hash = encode(sha256('relayops-admin'::bytea), 'hex'));

UPDATE admin_users
   SET password_hash = ''
 WHERE password_hash = encode(sha256('admin123'::bytea), 'hex');

UPDATE admin_users
   SET token_hash = encode(sha256((gen_random_uuid()::text || gen_random_uuid()::text || clock_timestamp()::text)::bytea), 'hex')
 WHERE token_hash = encode(sha256('relayops-admin'::bytea), 'hex');
