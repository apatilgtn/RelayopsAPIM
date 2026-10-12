-- One AI budget account per consumer, and one tenant-wide account per tenant.
-- Earlier "upserts" inserted a new account on every save, and the gateway
-- then enforced whichever duplicate a lookup happened to return. Merge each
-- group into the account with the highest recorded spend (the conservative
-- choice for enforcement), carrying open reservations over, then make the
-- uniqueness explicit so upserts can update in place.
WITH ranked AS (
    SELECT id,
           first_value(id) OVER (PARTITION BY tenant_id, consumer_id
                                 ORDER BY current_spend_cents DESC, updated_at DESC, id) AS keep_id
    FROM ai_budget_accounts
), dupes AS (
    SELECT id, keep_id FROM ranked WHERE id <> keep_id
), moved AS (
    UPDATE ai_budget_reservations r SET account_id = d.keep_id
    FROM dupes d WHERE r.account_id = d.id
    RETURNING d.keep_id, r.reserved_cents, r.status
), carried AS (
    SELECT keep_id, sum(reserved_cents) AS cents FROM moved WHERE status = 'pending' GROUP BY keep_id
)
UPDATE ai_budget_accounts a SET reserved_spend_cents = a.reserved_spend_cents + c.cents, updated_at = now()
FROM carried c WHERE a.id = c.keep_id;

DELETE FROM ai_budget_accounts a
USING (
    SELECT id, first_value(id) OVER (PARTITION BY tenant_id, consumer_id
                                     ORDER BY current_spend_cents DESC, updated_at DESC, id) AS keep_id
    FROM ai_budget_accounts
) r
WHERE a.id = r.id AND r.id <> r.keep_id;

CREATE UNIQUE INDEX IF NOT EXISTS ai_budget_accounts_consumer_uidx
    ON ai_budget_accounts (tenant_id, consumer_id) WHERE consumer_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS ai_budget_accounts_tenant_wide_uidx
    ON ai_budget_accounts (tenant_id) WHERE consumer_id IS NULL;
