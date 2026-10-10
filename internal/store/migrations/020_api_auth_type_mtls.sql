-- 020_api_auth_type_mtls.sql
-- Allow auth_type 'mtls'. The gateway now derives client-certificate identity
-- only from a certificate it verified or a trusted ingress forwarded, and
-- discards client-sent certificate headers, so mTLS APIs can be offered.

ALTER TABLE apis DROP CONSTRAINT IF EXISTS apis_auth_type_check;
ALTER TABLE apis ADD CONSTRAINT apis_auth_type_check CHECK (auth_type IN ('none', 'api_key', 'jwt', 'oidc', 'mtls'));
