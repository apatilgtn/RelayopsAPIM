-- Self-service signups were meant to be created inactive, awaiting approval,
-- but account creation forced every account active. Such accounts never
-- reached the Approvals queue and could sign in unapproved. Return the ones
-- that were never approved, edited or used to the queue.
UPDATE admin_users u SET active = false, updated_at = now()
WHERE u.active
  AND u.sso_provider = 'local'
  AND u.last_login_at IS NULL
  AND EXISTS (SELECT 1 FROM audit_logs a
              WHERE a.resource_type = 'admin_user' AND a.resource_id = u.id::text AND a.action = 'REQUEST_ACCESS')
  AND NOT EXISTS (SELECT 1 FROM audit_logs a
                  WHERE a.resource_type = 'admin_user' AND a.resource_id = u.id::text AND a.action <> 'REQUEST_ACCESS');
