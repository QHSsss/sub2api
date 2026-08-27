package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAffiliateRebateSourcesMigrationPreservesAndClassifiesLedgerRows(t *testing.T) {
	content, err := FS.ReadFile("231_affiliate_rebate_sources.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS source_type VARCHAR(32) NULL")
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS base_amount DECIMAL(20,8) NULL")
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS source_redeem_code_id BIGINT NULL REFERENCES redeem_codes(id)")
	require.Contains(t, sql, "ledger_candidate_count = 1")
	require.Contains(t, sql, "redeem_candidate_count = 1")
	require.Contains(t, sql, "invitee_aff.inviter_id = ual.user_id")
	require.Contains(t, sql, "po.recharge_code = rc.code")
	require.Contains(t, sql, "payment_order_id IS NOT NULL OR redeem_code_type = 'admin_balance'")
	require.Contains(t, sql, "source_order_id = unique_matches.payment_order_id")
	require.Contains(t, sql, "SET source_type = 'legacy_unknown'")
	require.Contains(t, sql, "idx_user_affiliate_ledger_accrue_order_uniq")
	require.Contains(t, sql, "idx_user_affiliate_ledger_accrue_redeem_code_uniq")
	require.NotContains(t, strings.ToUpper(sql), "DELETE FROM USER_AFFILIATE_LEDGER")
}
