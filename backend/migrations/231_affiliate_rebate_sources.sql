-- 为邀请返利流水补充统一来源，覆盖支付订单、余额兑换码和管理员充值。
ALTER TABLE user_affiliate_ledger
    ADD COLUMN IF NOT EXISTS source_type VARCHAR(32) NULL;

ALTER TABLE user_affiliate_ledger
    ADD COLUMN IF NOT EXISTS base_amount DECIMAL(20,8) NULL;

ALTER TABLE user_affiliate_ledger
    ADD COLUMN IF NOT EXISTS source_redeem_code_id BIGINT NULL REFERENCES redeem_codes(id) ON DELETE SET NULL;

COMMENT ON COLUMN user_affiliate_ledger.source_type IS '返利来源：payment_order|balance_redeem_code|admin_recharge|legacy_unknown；转余额流水为 NULL';
COMMENT ON COLUMN user_affiliate_ledger.base_amount IS '计算该笔返利时使用的充值金额快照';
COMMENT ON COLUMN user_affiliate_ledger.source_redeem_code_id IS '产生返利的余额兑换码或管理员余额调整记录';

-- 已有关联订单的返利可以可靠识别为支付订单来源。
UPDATE user_affiliate_ledger ual
SET source_type = 'payment_order',
    base_amount = COALESCE(ual.base_amount, po.amount),
    updated_at = NOW()
FROM payment_orders po
WHERE ual.action = 'accrue'
  AND ual.source_order_id = po.id
  AND (
      ual.source_type IS DISTINCT FROM 'payment_order'
      OR ual.base_amount IS NULL
  );

-- 对没有订单关联的历史流水，仅在流水与兑换记录能够一对一匹配时回填。
-- 旧版支付充值也会生成余额兑换码；能通过充值码和用户唯一关联到已完成余额订单时，
-- 仍归为支付订单来源，避免在历史筛选里误算成手工兑换码。
-- 管理员充值历史流水发生在 admin_balance 调整记录创建之前；普通余额兑换码没有
-- 可证明来源的历史业务编号，因此不做猜测，后续统一归入 legacy_unknown。
WITH candidates AS (
    SELECT ual.id AS ledger_id,
           rc.id AS redeem_code_id,
           rc.type AS redeem_code_type,
           po.id AS payment_order_id,
           COALESCE(po.amount, rc.value) AS base_amount,
           COUNT(*) OVER (PARTITION BY ual.id) AS ledger_candidate_count,
           COUNT(*) OVER (PARTITION BY rc.id) AS redeem_candidate_count
    FROM user_affiliate_ledger ual
    JOIN user_affiliates invitee_aff
      ON invitee_aff.user_id = ual.source_user_id
     AND invitee_aff.inviter_id = ual.user_id
    JOIN redeem_codes rc
      ON rc.used_by = ual.source_user_id
     AND rc.status = 'used'
     AND rc.type IN ('balance', 'admin_balance')
     AND rc.value > 0
     AND rc.used_at IS NOT NULL
     AND (
         (rc.type = 'admin_balance'
          AND ual.created_at BETWEEN rc.used_at - INTERVAL '10 minutes' AND rc.used_at)
         OR
         (rc.type = 'balance'
          AND ual.created_at BETWEEN rc.used_at AND rc.used_at + INTERVAL '10 minutes')
     )
    LEFT JOIN payment_orders po
      ON po.recharge_code = rc.code
     AND po.user_id = ual.source_user_id
     AND po.order_type = 'balance'
     AND po.status = 'COMPLETED'
    WHERE ual.action = 'accrue'
      AND ual.source_order_id IS NULL
      AND ual.source_redeem_code_id IS NULL
      AND ual.source_type IS NULL
), unique_matches AS (
    SELECT ledger_id, redeem_code_id, redeem_code_type, payment_order_id, base_amount
    FROM candidates
    WHERE ledger_candidate_count = 1
      AND redeem_candidate_count = 1
      AND (payment_order_id IS NOT NULL OR redeem_code_type = 'admin_balance')
)
UPDATE user_affiliate_ledger ual
SET source_type = CASE
        WHEN unique_matches.payment_order_id IS NOT NULL THEN 'payment_order'
        ELSE 'admin_recharge'
    END,
    source_order_id = unique_matches.payment_order_id,
    source_redeem_code_id = CASE
        WHEN unique_matches.payment_order_id IS NULL THEN unique_matches.redeem_code_id
        ELSE NULL
    END,
    base_amount = unique_matches.base_amount,
    updated_at = NOW()
FROM unique_matches
WHERE ual.id = unique_matches.ledger_id;

-- 无法可靠反推来源的历史返利继续保留，明确归入历史未知，禁止猜测资金来源。
UPDATE user_affiliate_ledger
SET source_type = 'legacy_unknown',
    updated_at = NOW()
WHERE action = 'accrue'
  AND source_type IS NULL;

ALTER TABLE user_affiliate_ledger
    DROP CONSTRAINT IF EXISTS chk_user_affiliate_ledger_source_type;

ALTER TABLE user_affiliate_ledger
    ADD CONSTRAINT chk_user_affiliate_ledger_source_type CHECK (
        (action = 'accrue' AND source_type IN (
            'payment_order',
            'balance_redeem_code',
            'admin_recharge',
            'legacy_unknown'
        ))
        OR (action <> 'accrue' AND source_type IS NULL)
    );

CREATE INDEX IF NOT EXISTS idx_user_affiliate_ledger_source_type_created_at
    ON user_affiliate_ledger(source_type, created_at DESC)
    WHERE action = 'accrue';

-- 一个真实充值来源最多产生一笔返利。若历史数据已经重复，迁移应失败并由人工核账，
-- 禁止自动删除或合并资金流水。
CREATE UNIQUE INDEX IF NOT EXISTS idx_user_affiliate_ledger_accrue_order_uniq
    ON user_affiliate_ledger(source_order_id)
    WHERE action = 'accrue' AND source_order_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_user_affiliate_ledger_accrue_redeem_code_uniq
    ON user_affiliate_ledger(source_redeem_code_id)
    WHERE action = 'accrue' AND source_redeem_code_id IS NOT NULL;
