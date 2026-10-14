-- Plans a developer may choose for themselves in the developer portal.
-- Until now the portal listed every plan and accepted any plan ID at
-- self-registration, so a self-registered developer could pick an internal
-- "Unlimited" plan. Default off; only plans named "Free" are opted in, which
-- matches the plan the portal already assigns by default.
ALTER TABLE plans ADD COLUMN IF NOT EXISTS self_service BOOLEAN NOT NULL DEFAULT false;
UPDATE plans SET self_service = true WHERE lower(name) = 'free';
