//go:build integration

package repository

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	migrationspkg "github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

type failingAccrueAffiliateRepository struct {
	service.AffiliateRepository
	err error
}

func (r *failingAccrueAffiliateRepository) AccrueQuota(context.Context, service.AffiliateAccrualInput) (float64, error) {
	return 0, r.err
}

func newAdminServiceForAffiliateIntegration(
	userRepo service.UserRepository,
	redeemRepo service.RedeemCodeRepository,
	client *dbent.Client,
	settingService *service.SettingService,
	affiliateService *service.AffiliateService,
) service.AdminService {
	return service.NewAdminService(
		userRepo,
		nil,
		nil,
		nil,
		nil,
		redeemRepo,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		client,
		settingService,
		nil,
		nil,
		nil,
		nil,
		affiliateService,
		nil,
		nil,
		nil,
	)
}

func querySingleFloat(t *testing.T, ctx context.Context, client *dbent.Client, query string, args ...any) float64 {
	t.Helper()
	rows, err := client.QueryContext(ctx, query, args...)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	require.True(t, rows.Next(), "expected one row")
	var value float64
	require.NoError(t, rows.Scan(&value))
	require.NoError(t, rows.Err())
	return value
}

func querySingleInt(t *testing.T, ctx context.Context, client *dbent.Client, query string, args ...any) int {
	t.Helper()
	rows, err := client.QueryContext(ctx, query, args...)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	require.True(t, rows.Next(), "expected one row")
	var value int
	require.NoError(t, rows.Scan(&value))
	require.NoError(t, rows.Err())
	return value
}

func TestAffiliateRepository_TransferQuotaToBalance_UsesClaimedQuotaBeforeClear(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	txCtx := dbent.NewTxContext(ctx, tx)
	client := tx.Client()

	repo := NewAffiliateRepository(client, integrationDB)

	u := mustCreateUser(t, client, &service.User{
		Email:        fmt.Sprintf("affiliate-transfer-%d@example.com", time.Now().UnixNano()),
		PasswordHash: "hash",
		Role:         service.RoleUser,
		Status:       service.StatusActive,
		Balance:      5.5,
		Concurrency:  5,
	})

	affCode := fmt.Sprintf("AFF%09d", time.Now().UnixNano()%1_000_000_000)
	_, err := client.ExecContext(txCtx, `
INSERT INTO user_affiliates (user_id, aff_code, aff_quota, aff_history_quota, created_at, updated_at)
VALUES ($1, $2, $3, $3, NOW(), NOW())`, u.ID, affCode, 12.34)
	require.NoError(t, err)

	transferred, balance, err := repo.TransferQuotaToBalance(txCtx, u.ID)
	require.NoError(t, err)
	require.InDelta(t, 12.34, transferred, 1e-9)
	require.InDelta(t, 17.84, balance, 1e-9)

	affQuota := querySingleFloat(t, txCtx, client,
		"SELECT aff_quota::double precision FROM user_affiliates WHERE user_id = $1", u.ID)
	require.InDelta(t, 0.0, affQuota, 1e-9)

	persistedBalance := querySingleFloat(t, txCtx, client,
		"SELECT balance::double precision FROM users WHERE id = $1", u.ID)
	require.InDelta(t, 17.84, persistedBalance, 1e-9)

	ledgerCount := querySingleInt(t, txCtx, client,
		"SELECT COUNT(*) FROM user_affiliate_ledger WHERE user_id = $1 AND action = 'transfer'", u.ID)
	require.Equal(t, 1, ledgerCount)

	rows, err := client.QueryContext(txCtx, `
SELECT amount::double precision,
       balance_after::double precision,
       aff_quota_after::double precision,
       aff_frozen_quota_after::double precision,
       aff_history_quota_after::double precision
FROM user_affiliate_ledger
WHERE user_id = $1 AND action = 'transfer'
LIMIT 1`, u.ID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	require.True(t, rows.Next(), "expected transfer ledger")
	var amount, balanceAfter, quotaAfter, frozenAfter, historyAfter float64
	require.NoError(t, rows.Scan(&amount, &balanceAfter, &quotaAfter, &frozenAfter, &historyAfter))
	require.InDelta(t, 12.34, amount, 1e-9)
	require.InDelta(t, 17.84, balanceAfter, 1e-9)
	require.InDelta(t, 0.0, quotaAfter, 1e-9)
	require.InDelta(t, 0.0, frozenAfter, 1e-9)
	require.InDelta(t, 12.34, historyAfter, 1e-9)
}

// TestAffiliateRepository_AccrueQuota_ReusesOuterTransaction guards the
// cross-layer tx propagation invariant: when AccrueQuota is called with a ctx
// that already carries a transaction (via dbent.NewTxContext), repo.withTx
// must reuse that tx rather than opening a nested one. If this invariant
// breaks, AccrueQuota would commit independently and survive a rollback of
// the outer tx, which would violate payment_fulfillment's all-or-nothing
// semantics.
func TestAffiliateRepository_AccrueQuota_ReusesOuterTransaction(t *testing.T) {
	ctx := context.Background()

	outerTx, err := integrationEntClient.Tx(ctx)
	require.NoError(t, err, "begin outer tx")
	// Defensive cleanup: if any require.* below fires before the explicit
	// Rollback, this prevents the tx from leaking until container teardown.
	// Rollback is idempotent at the driver level (extra rollback returns an
	// error we ignore).
	t.Cleanup(func() { _ = outerTx.Rollback() })
	client := outerTx.Client()
	txCtx := dbent.NewTxContext(ctx, outerTx)

	inviter := mustCreateUser(t, client, &service.User{
		Email:        fmt.Sprintf("affiliate-inviter-%d@example.com", time.Now().UnixNano()),
		PasswordHash: "hash",
		Role:         service.RoleUser,
		Status:       service.StatusActive,
		Concurrency:  5,
	})
	invitee := mustCreateUser(t, client, &service.User{
		Email:        fmt.Sprintf("affiliate-invitee-%d@example.com", time.Now().UnixNano()+1),
		PasswordHash: "hash",
		Role:         service.RoleUser,
		Status:       service.StatusActive,
		Concurrency:  5,
	})

	repo := NewAffiliateRepository(client, integrationDB)
	_, err = repo.EnsureUserAffiliate(txCtx, inviter.ID)
	require.NoError(t, err)
	_, err = repo.EnsureUserAffiliate(txCtx, invitee.ID)
	require.NoError(t, err)

	bound, err := repo.BindInviter(txCtx, invitee.ID, inviter.ID)
	require.NoError(t, err)
	require.True(t, bound, "invitee must bind to inviter")

	applied, err := repo.AccrueQuota(txCtx, service.AffiliateAccrualInput{
		InviterID:     inviter.ID,
		InviteeUserID: invitee.ID,
		Amount:        3.5,
		Source: service.AffiliateRebateSource{
			Type:       service.AffiliateRebateSourceAdminRecharge,
			BaseAmount: 17.5,
		},
	})
	require.NoError(t, err)
	require.InDelta(t, 3.5, applied, 1e-9, "AccrueQuota must return the applied amount")

	// Visible inside the outer tx.
	innerQuota := querySingleFloat(t, txCtx, client,
		"SELECT aff_quota::double precision FROM user_affiliates WHERE user_id = $1", inviter.ID)
	require.InDelta(t, 3.5, innerQuota, 1e-9)

	// Roll back the outer tx; if AccrueQuota had opened its own inner tx and
	// committed it, the rows would still be visible to the global client.
	require.NoError(t, outerTx.Rollback())

	rows, err := integrationEntClient.QueryContext(ctx,
		"SELECT COUNT(*) FROM user_affiliates WHERE user_id IN ($1, $2)",
		inviter.ID, invitee.ID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	require.True(t, rows.Next())
	var postRollbackCount int
	require.NoError(t, rows.Scan(&postRollbackCount))
	require.Equal(t, 0, postRollbackCount,
		"AccrueQuota must propagate the outer tx — found persisted rows after rollback")
}

func TestAffiliateRepository_AccrueQuota_RedeemSourceIsIdempotentAndListable(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	txCtx := dbent.NewTxContext(ctx, tx)
	client := tx.Client()
	repo := NewAffiliateRepository(client, integrationDB)

	inviter := mustCreateUser(t, client, &service.User{
		Email:        fmt.Sprintf("affiliate-redeem-inviter-%d@example.com", time.Now().UnixNano()),
		PasswordHash: "hash",
		Role:         service.RoleUser,
		Status:       service.StatusActive,
	})
	invitee := mustCreateUser(t, client, &service.User{
		Email:        fmt.Sprintf("affiliate-redeem-invitee-%d@example.com", time.Now().UnixNano()+1),
		PasswordHash: "hash",
		Role:         service.RoleUser,
		Status:       service.StatusActive,
	})
	_, err := repo.EnsureUserAffiliate(txCtx, inviter.ID)
	require.NoError(t, err)
	_, err = repo.EnsureUserAffiliate(txCtx, invitee.ID)
	require.NoError(t, err)
	bound, err := repo.BindInviter(txCtx, invitee.ID, inviter.ID)
	require.NoError(t, err)
	require.True(t, bound)

	shortRedeemCode := fmt.Sprintf("%04X", time.Now().UnixNano()&0xffff)
	rows, err := client.QueryContext(txCtx, `
INSERT INTO redeem_codes (code, type, value, status, used_by, used_at, created_at)
VALUES ($1, 'balance', 50, 'used', $2, NOW(), NOW())
RETURNING id`, shortRedeemCode, invitee.ID)
	require.NoError(t, err)
	require.True(t, rows.Next())
	var redeemCodeID int64
	require.NoError(t, rows.Scan(&redeemCodeID))
	require.NoError(t, rows.Close())

	input := service.AffiliateAccrualInput{
		InviterID:     inviter.ID,
		InviteeUserID: invitee.ID,
		Amount:        10,
		Source: service.AffiliateRebateSource{
			Type:         service.AffiliateRebateSourceBalanceRedeem,
			BaseAmount:   50,
			RedeemCodeID: &redeemCodeID,
		},
	}
	first, err := repo.AccrueQuota(txCtx, input)
	require.NoError(t, err)
	require.InDelta(t, 10, first, 1e-9)
	second, err := repo.AccrueQuota(txCtx, input)
	require.NoError(t, err)
	require.Zero(t, second)

	quota := querySingleFloat(t, txCtx, client,
		"SELECT aff_quota::double precision FROM user_affiliates WHERE user_id = $1", inviter.ID)
	require.InDelta(t, 10, quota, 1e-9)
	ledgerCount := querySingleInt(t, txCtx, client,
		"SELECT COUNT(*) FROM user_affiliate_ledger WHERE source_redeem_code_id = $1", redeemCodeID)
	require.Equal(t, 1, ledgerCount)

	items, total, err := repo.ListAffiliateRebateRecords(txCtx, service.AffiliateRecordFilter{
		SourceType: string(service.AffiliateRebateSourceBalanceRedeem),
		Page:       1,
		PageSize:   20,
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Len(t, items, 1)
	require.Equal(t, string(service.AffiliateRebateSourceBalanceRedeem), items[0].SourceType)
	require.Equal(t, redeemCodeID, *items[0].RedeemCodeID)
	require.Equal(t, "****", *items[0].RedeemCodeMasked)
	require.NotContains(t, *items[0].RedeemCodeMasked, shortRedeemCode)
	require.InDelta(t, 50, *items[0].BaseAmount, 1e-9)
	require.InDelta(t, 10, items[0].RebateAmount, 1e-9)
}

func TestAffiliateRepository_AccrueQuota_ConcurrentSameRedeemSourceIsIdempotent(t *testing.T) {
	ctx := context.Background()
	repo := NewAffiliateRepository(integrationEntClient, integrationDB)

	inviter := mustCreateUser(t, integrationEntClient, &service.User{
		Email:        fmt.Sprintf("affiliate-source-race-inviter-%d@example.com", time.Now().UnixNano()),
		PasswordHash: "hash",
		Role:         service.RoleUser,
		Status:       service.StatusActive,
	})
	invitee := mustCreateUser(t, integrationEntClient, &service.User{
		Email:        fmt.Sprintf("affiliate-source-race-invitee-%d@example.com", time.Now().UnixNano()+1),
		PasswordHash: "hash",
		Role:         service.RoleUser,
		Status:       service.StatusActive,
	})
	_, err := repo.EnsureUserAffiliate(ctx, inviter.ID)
	require.NoError(t, err)
	_, err = repo.EnsureUserAffiliate(ctx, invitee.ID)
	require.NoError(t, err)

	rows, err := integrationEntClient.QueryContext(ctx, `
INSERT INTO redeem_codes (code, type, value, status, used_by, used_at, created_at)
VALUES ($1, 'balance', 50, 'used', $2, NOW(), NOW())
RETURNING id`, fmt.Sprintf("RACE%d", time.Now().UnixNano()), invitee.ID)
	require.NoError(t, err)
	require.True(t, rows.Next())
	var redeemCodeID int64
	require.NoError(t, rows.Scan(&redeemCodeID))
	require.NoError(t, rows.Close())

	start := make(chan struct{})
	results := make(chan struct {
		amount float64
		err    error
	}, 2)
	for range 2 {
		go func() {
			<-start
			amount, accrueErr := repo.AccrueQuota(ctx, service.AffiliateAccrualInput{
				InviterID:     inviter.ID,
				InviteeUserID: invitee.ID,
				Amount:        10,
				Source: service.AffiliateRebateSource{
					Type:         service.AffiliateRebateSourceBalanceRedeem,
					BaseAmount:   50,
					RedeemCodeID: &redeemCodeID,
				},
			})
			results <- struct {
				amount float64
				err    error
			}{amount: amount, err: accrueErr}
		}()
	}
	close(start)

	var appliedTotal float64
	for range 2 {
		result := <-results
		require.NoError(t, result.err)
		appliedTotal += result.amount
	}
	require.InDelta(t, 10, appliedTotal, 1e-9)
	ledgerCount := querySingleInt(t, ctx, integrationEntClient,
		"SELECT COUNT(*) FROM user_affiliate_ledger WHERE source_redeem_code_id = $1", redeemCodeID)
	require.Equal(t, 1, ledgerCount)
	quota := querySingleFloat(t, ctx, integrationEntClient,
		"SELECT aff_quota::double precision FROM user_affiliates WHERE user_id = $1", inviter.ID)
	require.InDelta(t, 10, quota, 1e-9)
}

func TestAffiliateRepository_AccrueQuota_TruncatesAtPerInviteeCap(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	txCtx := dbent.NewTxContext(ctx, tx)
	client := tx.Client()
	repo := NewAffiliateRepository(client, integrationDB)

	inviter := mustCreateUser(t, client, &service.User{
		Email:        fmt.Sprintf("affiliate-cap-inviter-%d@example.com", time.Now().UnixNano()),
		PasswordHash: "hash",
		Role:         service.RoleUser,
		Status:       service.StatusActive,
	})
	invitee := mustCreateUser(t, client, &service.User{
		Email:        fmt.Sprintf("affiliate-cap-invitee-%d@example.com", time.Now().UnixNano()+1),
		PasswordHash: "hash",
		Role:         service.RoleUser,
		Status:       service.StatusActive,
	})
	_, err := repo.EnsureUserAffiliate(txCtx, inviter.ID)
	require.NoError(t, err)
	_, err = repo.EnsureUserAffiliate(txCtx, invitee.ID)
	require.NoError(t, err)

	input := service.AffiliateAccrualInput{
		InviterID:     inviter.ID,
		InviteeUserID: invitee.ID,
		Amount:        4.12345678,
		PerInviteeCap: 5.000000009,
		Source: service.AffiliateRebateSource{
			Type:       service.AffiliateRebateSourceAdminRecharge,
			BaseAmount: 20,
		},
	}
	first, err := repo.AccrueQuota(txCtx, input)
	require.NoError(t, err)
	require.InDelta(t, 4.12345678, first, 1e-9)
	second, err := repo.AccrueQuota(txCtx, input)
	require.NoError(t, err)
	require.InDelta(t, 0.87654322, second, 1e-9)
	third, err := repo.AccrueQuota(txCtx, input)
	require.NoError(t, err)
	require.Zero(t, third)

	quota := querySingleFloat(t, txCtx, client,
		"SELECT aff_quota::double precision FROM user_affiliates WHERE user_id = $1", inviter.ID)
	require.InDelta(t, 5, quota, 1e-9)
}

func TestAffiliateRepository_AccrueQuota_ConcurrentRequestsRespectPerInviteeCap(t *testing.T) {
	ctx := context.Background()
	repo := NewAffiliateRepository(integrationEntClient, integrationDB)

	inviter := mustCreateUser(t, integrationEntClient, &service.User{
		Email:        fmt.Sprintf("affiliate-concurrent-cap-inviter-%d@example.com", time.Now().UnixNano()),
		PasswordHash: "hash",
		Role:         service.RoleUser,
		Status:       service.StatusActive,
	})
	invitee := mustCreateUser(t, integrationEntClient, &service.User{
		Email:        fmt.Sprintf("affiliate-concurrent-cap-invitee-%d@example.com", time.Now().UnixNano()+1),
		PasswordHash: "hash",
		Role:         service.RoleUser,
		Status:       service.StatusActive,
	})
	_, err := repo.EnsureUserAffiliate(ctx, inviter.ID)
	require.NoError(t, err)
	_, err = repo.EnsureUserAffiliate(ctx, invitee.ID)
	require.NoError(t, err)

	type result struct {
		amount float64
		err    error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			amount, accrueErr := repo.AccrueQuota(ctx, service.AffiliateAccrualInput{
				InviterID:     inviter.ID,
				InviteeUserID: invitee.ID,
				Amount:        4,
				PerInviteeCap: 5,
				Source: service.AffiliateRebateSource{
					Type:       service.AffiliateRebateSourceAdminRecharge,
					BaseAmount: 20,
				},
			})
			results <- result{amount: amount, err: accrueErr}
		}()
	}
	close(start)

	var appliedTotal float64
	for range 2 {
		result := <-results
		require.NoError(t, result.err)
		appliedTotal += result.amount
	}
	require.InDelta(t, 5, appliedTotal, 1e-9)

	quota := querySingleFloat(t, ctx, integrationEntClient,
		"SELECT aff_quota::double precision FROM user_affiliates WHERE user_id = $1", inviter.ID)
	require.InDelta(t, 5, quota, 1e-9)
	ledgerTotal := querySingleFloat(t, ctx, integrationEntClient,
		"SELECT COALESCE(SUM(amount), 0)::double precision FROM user_affiliate_ledger WHERE user_id = $1 AND source_user_id = $2 AND action = 'accrue'",
		inviter.ID, invitee.ID)
	require.InDelta(t, 5, ledgerTotal, 1e-9)
}

func TestAdminService_UpdateUserBalance_AdminRechargeCommitsOrRollsBackAsOneTransaction(t *testing.T) {
	ctx := context.Background()
	userRepo := NewUserRepository(integrationEntClient, integrationDB)
	redeemRepo := NewRedeemCodeRepository(integrationEntClient)
	affiliateRepo := NewAffiliateRepository(integrationEntClient, integrationDB)
	settingRepo := NewSettingRepository(integrationEntClient)
	require.NoError(t, settingRepo.Set(ctx, service.SettingKeyAffiliateEnabled, "true"))
	require.NoError(t, settingRepo.Set(ctx, service.SettingKeyAffiliateAdminRechargeEnabled, "true"))
	require.NoError(t, settingRepo.Set(ctx, service.SettingKeyAffiliateRebateRate, "20"))
	settingService := service.NewSettingService(settingRepo, nil)

	inviter := mustCreateUser(t, integrationEntClient, &service.User{
		Email:        fmt.Sprintf("admin-recharge-rollback-inviter-%d@example.com", time.Now().UnixNano()),
		PasswordHash: "hash",
		Role:         service.RoleUser,
		Status:       service.StatusActive,
	})
	invitee := mustCreateUser(t, integrationEntClient, &service.User{
		Email:        fmt.Sprintf("admin-recharge-rollback-invitee-%d@example.com", time.Now().UnixNano()+1),
		PasswordHash: "hash",
		Role:         service.RoleUser,
		Status:       service.StatusActive,
		Balance:      10,
	})
	_, err := affiliateRepo.EnsureUserAffiliate(ctx, inviter.ID)
	require.NoError(t, err)
	_, err = affiliateRepo.EnsureUserAffiliate(ctx, invitee.ID)
	require.NoError(t, err)
	bound, err := affiliateRepo.BindInviter(ctx, invitee.ID, inviter.ID)
	require.NoError(t, err)
	require.True(t, bound)

	successfulAffiliateService := service.NewAffiliateService(affiliateRepo, settingService, nil, nil)
	successfulAdminService := newAdminServiceForAffiliateIntegration(
		userRepo, redeemRepo, integrationEntClient, settingService, successfulAffiliateService,
	)
	updated, err := successfulAdminService.UpdateUserBalance(ctx, invitee.ID, 5, "add", "commit-test")
	require.NoError(t, err)
	require.InDelta(t, 15, updated.Balance, 1e-9)

	rows, err := integrationEntClient.QueryContext(ctx, `
SELECT rc.id,
       ual.source_type,
       ual.base_amount::double precision,
       ual.amount::double precision
FROM redeem_codes rc
JOIN user_affiliate_ledger ual ON ual.source_redeem_code_id = rc.id
WHERE rc.used_by = $1 AND rc.type = 'admin_balance'`, invitee.ID)
	require.NoError(t, err)
	require.True(t, rows.Next())
	var adjustmentRecordID int64
	var sourceType string
	var baseAmount, rebateAmount float64
	require.NoError(t, rows.Scan(&adjustmentRecordID, &sourceType, &baseAmount, &rebateAmount))
	require.NoError(t, rows.Close())
	require.Positive(t, adjustmentRecordID)
	require.Equal(t, string(service.AffiliateRebateSourceAdminRecharge), sourceType)
	require.InDelta(t, 5, baseAmount, 1e-9)
	require.InDelta(t, 1, rebateAmount, 1e-9)

	failingAffiliateService := service.NewAffiliateService(&failingAccrueAffiliateRepository{
		AffiliateRepository: affiliateRepo,
		err:                 errors.New("forced affiliate failure"),
	}, settingService, nil, nil)
	failingAdminService := newAdminServiceForAffiliateIntegration(
		userRepo, redeemRepo, integrationEntClient, settingService, failingAffiliateService,
	)

	updated, err = failingAdminService.UpdateUserBalance(ctx, invitee.ID, 5, "add", "rollback-test")
	require.Nil(t, updated)
	require.ErrorContains(t, err, "forced affiliate failure")

	balance := querySingleFloat(t, ctx, integrationEntClient,
		"SELECT balance::double precision FROM users WHERE id = $1", invitee.ID)
	require.InDelta(t, 15, balance, 1e-9)
	adjustmentCount := querySingleInt(t, ctx, integrationEntClient,
		"SELECT COUNT(*) FROM redeem_codes WHERE used_by = $1 AND type = 'admin_balance'", invitee.ID)
	require.Equal(t, 1, adjustmentCount)
	ledgerCount := querySingleInt(t, ctx, integrationEntClient,
		"SELECT COUNT(*) FROM user_affiliate_ledger WHERE user_id = $1 AND source_user_id = $2 AND action = 'accrue'",
		inviter.ID, invitee.ID)
	require.Equal(t, 1, ledgerCount)
}

func TestAffiliateRebateSourcesMigration_ConservativeBackfillAndRepeatExecution(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	txCtx := dbent.NewTxContext(ctx, tx)
	client := tx.Client()
	repo := NewAffiliateRepository(client, integrationDB)

	inviter := mustCreateUser(t, client, &service.User{
		Email:        fmt.Sprintf("migration-source-inviter-%d@example.com", time.Now().UnixNano()),
		PasswordHash: "hash",
		Role:         service.RoleUser,
		Status:       service.StatusActive,
	})
	ordinaryInvitee := mustCreateUser(t, client, &service.User{
		Email:        fmt.Sprintf("migration-source-ordinary-%d@example.com", time.Now().UnixNano()+1),
		PasswordHash: "hash",
		Role:         service.RoleUser,
		Status:       service.StatusActive,
	})
	adminInvitee := mustCreateUser(t, client, &service.User{
		Email:        fmt.Sprintf("migration-source-admin-%d@example.com", time.Now().UnixNano()+2),
		PasswordHash: "hash",
		Role:         service.RoleUser,
		Status:       service.StatusActive,
	})
	_, err := repo.EnsureUserAffiliate(txCtx, inviter.ID)
	require.NoError(t, err)
	for _, inviteeID := range []int64{ordinaryInvitee.ID, adminInvitee.ID} {
		_, err = repo.EnsureUserAffiliate(txCtx, inviteeID)
		require.NoError(t, err)
		bound, bindErr := repo.BindInviter(txCtx, inviteeID, inviter.ID)
		require.NoError(t, bindErr)
		require.True(t, bound)
	}

	_, err = client.ExecContext(txCtx, "ALTER TABLE user_affiliate_ledger DROP CONSTRAINT IF EXISTS chk_user_affiliate_ledger_source_type")
	require.NoError(t, err)
	now := time.Now().UTC()
	ordinaryCodeID := insertHistoricalRedeemCode(t, txCtx, client, ordinaryInvitee.ID, "balance", 50, now.Add(-time.Minute))
	ordinaryLedgerID := insertHistoricalAffiliateLedger(t, txCtx, client, inviter.ID, ordinaryInvitee.ID, 10, now)
	adminCodeID := insertHistoricalRedeemCode(t, txCtx, client, adminInvitee.ID, "admin_balance", 25, now)
	adminLedgerID := insertHistoricalAffiliateLedger(t, txCtx, client, inviter.ID, adminInvitee.ID, 5, now.Add(-time.Minute))

	migrationSQL, err := migrationspkg.FS.ReadFile("231_affiliate_rebate_sources.sql")
	require.NoError(t, err)
	for range 2 {
		_, err = client.ExecContext(txCtx, string(migrationSQL))
		require.NoError(t, err)
	}

	rows, err := client.QueryContext(txCtx, `
SELECT source_type, source_redeem_code_id
FROM user_affiliate_ledger
WHERE id = $1`, ordinaryLedgerID)
	require.NoError(t, err)
	require.True(t, rows.Next())
	var ordinarySource string
	var ordinarySourceID *int64
	require.NoError(t, rows.Scan(&ordinarySource, &ordinarySourceID))
	require.NoError(t, rows.Close())
	require.Equal(t, string(service.AffiliateRebateSourceLegacyUnknown), ordinarySource)
	require.Nil(t, ordinarySourceID)

	rows, err = client.QueryContext(txCtx, `
SELECT source_type, source_redeem_code_id, base_amount::double precision
FROM user_affiliate_ledger
WHERE id = $1`, adminLedgerID)
	require.NoError(t, err)
	require.True(t, rows.Next())
	var adminSource string
	var adminSourceID int64
	var adminBaseAmount float64
	require.NoError(t, rows.Scan(&adminSource, &adminSourceID, &adminBaseAmount))
	require.NoError(t, rows.Close())
	require.Equal(t, string(service.AffiliateRebateSourceAdminRecharge), adminSource)
	require.Equal(t, adminCodeID, adminSourceID)
	require.InDelta(t, 25, adminBaseAmount, 1e-9)

	// 普通历史余额码故意不建立来源关系；变量保留用于确认测试数据确实已创建。
	require.Positive(t, ordinaryCodeID)
}

func insertHistoricalRedeemCode(t *testing.T, ctx context.Context, client *dbent.Client, userID int64, codeType string, value float64, usedAt time.Time) int64 {
	t.Helper()
	rows, err := client.QueryContext(ctx, `
INSERT INTO redeem_codes (code, type, value, status, used_by, used_at, created_at)
VALUES ($1, $2, $3, 'used', $4, $5, $5)
RETURNING id`, fmt.Sprintf("HIST%d", time.Now().UnixNano()), codeType, value, userID, usedAt)
	require.NoError(t, err)
	require.True(t, rows.Next())
	var id int64
	require.NoError(t, rows.Scan(&id))
	require.NoError(t, rows.Close())
	return id
}

func insertHistoricalAffiliateLedger(t *testing.T, ctx context.Context, client *dbent.Client, inviterID, inviteeID int64, amount float64, createdAt time.Time) int64 {
	t.Helper()
	rows, err := client.QueryContext(ctx, `
INSERT INTO user_affiliate_ledger (user_id, action, amount, source_user_id, created_at, updated_at)
VALUES ($1, 'accrue', $2, $3, $4, $4)
RETURNING id`, inviterID, amount, inviteeID, createdAt)
	require.NoError(t, err)
	require.True(t, rows.Next())
	var id int64
	require.NoError(t, rows.Scan(&id))
	require.NoError(t, rows.Close())
	return id
}

func TestAffiliateRepository_TransferQuotaToBalance_EmptyQuota(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	txCtx := dbent.NewTxContext(ctx, tx)
	client := tx.Client()

	repo := NewAffiliateRepository(client, integrationDB)

	u := mustCreateUser(t, client, &service.User{
		Email:        fmt.Sprintf("affiliate-empty-%d@example.com", time.Now().UnixNano()),
		PasswordHash: "hash",
		Role:         service.RoleUser,
		Status:       service.StatusActive,
		Balance:      3.21,
		Concurrency:  5,
	})

	affCode := fmt.Sprintf("AFF%09d", time.Now().UnixNano()%1_000_000_000)
	_, err := client.ExecContext(txCtx, `
INSERT INTO user_affiliates (user_id, aff_code, aff_quota, aff_history_quota, created_at, updated_at)
VALUES ($1, $2, 0, 0, NOW(), NOW())`, u.ID, affCode)
	require.NoError(t, err)

	transferred, balance, err := repo.TransferQuotaToBalance(txCtx, u.ID)
	require.ErrorIs(t, err, service.ErrAffiliateQuotaEmpty)
	require.InDelta(t, 0.0, transferred, 1e-9)
	require.InDelta(t, 0.0, balance, 1e-9)

	persistedBalance := querySingleFloat(t, txCtx, client,
		"SELECT balance::double precision FROM users WHERE id = $1", u.ID)
	require.InDelta(t, 3.21, persistedBalance, 1e-9)
}

// TestAffiliateRepository_AdminCustomCode covers the success path of admin
// invite-code rewrite + reset within a shared test transaction:
// - UpdateUserAffCode replaces aff_code, sets aff_code_custom=true, lookup works
// - the old code can no longer be found
// - ResetUserAffCode reverts aff_code_custom and assigns a new system-format code
//
// The conflict path (duplicate code → ErrAffiliateCodeTaken) lives in its own
// test because a unique-violation aborts the surrounding Postgres tx, which
// would poison subsequent assertions in the same transaction.
func TestAffiliateRepository_AdminCustomCode(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	txCtx := dbent.NewTxContext(ctx, tx)
	client := tx.Client()

	repo := NewAffiliateRepository(client, integrationDB)

	u := mustCreateUser(t, client, &service.User{
		Email:        fmt.Sprintf("affiliate-custom-%d@example.com", time.Now().UnixNano()),
		PasswordHash: "hash",
		Role:         service.RoleUser,
		Status:       service.StatusActive,
	})

	original, err := repo.EnsureUserAffiliate(txCtx, u.ID)
	require.NoError(t, err)
	require.False(t, original.AffCodeCustom, "system-generated codes start as non-custom")
	originalCode := original.AffCode

	// Rewrite to a custom code
	customCode := fmt.Sprintf("VIP%09d", time.Now().UnixNano()%1_000_000_000)
	require.NoError(t, repo.UpdateUserAffCode(txCtx, u.ID, customCode))

	updated, err := repo.EnsureUserAffiliate(txCtx, u.ID)
	require.NoError(t, err)
	require.Equal(t, customCode, updated.AffCode)
	require.True(t, updated.AffCodeCustom)

	// Lookup by new custom code finds the user
	byCode, err := repo.GetAffiliateByCode(txCtx, customCode)
	require.NoError(t, err)
	require.Equal(t, u.ID, byCode.UserID)

	// Old system code should no longer match
	_, err = repo.GetAffiliateByCode(txCtx, originalCode)
	require.ErrorIs(t, err, service.ErrAffiliateProfileNotFound)

	// Reset back to a fresh system code, clears custom flag
	newSysCode, err := repo.ResetUserAffCode(txCtx, u.ID)
	require.NoError(t, err)
	require.NotEqual(t, customCode, newSysCode)

	reset, err := repo.EnsureUserAffiliate(txCtx, u.ID)
	require.NoError(t, err)
	require.Equal(t, newSysCode, reset.AffCode)
	require.False(t, reset.AffCodeCustom)

	// The old custom code is now free again
	_, err = repo.GetAffiliateByCode(txCtx, customCode)
	require.ErrorIs(t, err, service.ErrAffiliateProfileNotFound)
}

// TestAffiliateRepository_AdminCustomCode_Conflict isolates the unique-violation
// path. PostgreSQL aborts the enclosing tx when a unique constraint fires, so
// this test must be the only assertion and run in its own tx — production
// callers each have their own outer tx, so this matches real behavior.
func TestAffiliateRepository_AdminCustomCode_Conflict(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	txCtx := dbent.NewTxContext(ctx, tx)
	client := tx.Client()

	repo := NewAffiliateRepository(client, integrationDB)

	taker := mustCreateUser(t, client, &service.User{
		Email:        fmt.Sprintf("affiliate-conflict-taker-%d@example.com", time.Now().UnixNano()),
		PasswordHash: "hash",
		Role:         service.RoleUser, Status: service.StatusActive,
	})
	requester := mustCreateUser(t, client, &service.User{
		Email:        fmt.Sprintf("affiliate-conflict-req-%d@example.com", time.Now().UnixNano()),
		PasswordHash: "hash",
		Role:         service.RoleUser, Status: service.StatusActive,
	})

	takenCode := fmt.Sprintf("HOT%09d", time.Now().UnixNano()%1_000_000_000)
	require.NoError(t, repo.UpdateUserAffCode(txCtx, taker.ID, takenCode))

	// Now requester tries to grab the same code → conflict.
	err := repo.UpdateUserAffCode(txCtx, requester.ID, takenCode)
	require.ErrorIs(t, err, service.ErrAffiliateCodeTaken)
}

// TestAffiliateRepository_AdminRebateRate covers per-user exclusive rate
// set/clear and the Batch variant including NULL semantics.
func TestAffiliateRepository_AdminRebateRate(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	txCtx := dbent.NewTxContext(ctx, tx)
	client := tx.Client()

	repo := NewAffiliateRepository(client, integrationDB)

	u1 := mustCreateUser(t, client, &service.User{
		Email:        fmt.Sprintf("affiliate-rate-%d-a@example.com", time.Now().UnixNano()),
		PasswordHash: "hash",
		Role:         service.RoleUser,
		Status:       service.StatusActive,
	})
	u2 := mustCreateUser(t, client, &service.User{
		Email:        fmt.Sprintf("affiliate-rate-%d-b@example.com", time.Now().UnixNano()),
		PasswordHash: "hash",
		Role:         service.RoleUser,
		Status:       service.StatusActive,
	})

	// Set exclusive rate for u1
	rate := 42.5
	require.NoError(t, repo.SetUserRebateRate(txCtx, u1.ID, &rate))

	got, err := repo.EnsureUserAffiliate(txCtx, u1.ID)
	require.NoError(t, err)
	require.NotNil(t, got.AffRebateRatePercent)
	require.InDelta(t, 42.5, *got.AffRebateRatePercent, 1e-9)

	// Clear exclusive rate
	require.NoError(t, repo.SetUserRebateRate(txCtx, u1.ID, nil))
	cleared, err := repo.EnsureUserAffiliate(txCtx, u1.ID)
	require.NoError(t, err)
	require.Nil(t, cleared.AffRebateRatePercent)

	// Batch set both users
	batchRate := 15.0
	require.NoError(t, repo.BatchSetUserRebateRate(txCtx, []int64{u1.ID, u2.ID}, &batchRate))

	for _, uid := range []int64{u1.ID, u2.ID} {
		v, err := repo.EnsureUserAffiliate(txCtx, uid)
		require.NoError(t, err)
		require.NotNil(t, v.AffRebateRatePercent)
		require.InDelta(t, 15.0, *v.AffRebateRatePercent, 1e-9)
	}

	// Batch clear
	require.NoError(t, repo.BatchSetUserRebateRate(txCtx, []int64{u1.ID, u2.ID}, nil))
	for _, uid := range []int64{u1.ID, u2.ID} {
		v, err := repo.EnsureUserAffiliate(txCtx, uid)
		require.NoError(t, err)
		require.Nil(t, v.AffRebateRatePercent)
	}
}

// TestAffiliateRepository_ListUsersWithCustomSettings verifies the admin list
// only includes users with at least one override applied.
func TestAffiliateRepository_ListUsersWithCustomSettings(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	txCtx := dbent.NewTxContext(ctx, tx)
	client := tx.Client()

	repo := NewAffiliateRepository(client, integrationDB)

	// User without any custom config — should NOT appear in the list.
	plainEmail := fmt.Sprintf("affiliate-plain-%d@example.com", time.Now().UnixNano())
	uPlain := mustCreateUser(t, client, &service.User{
		Email: plainEmail, PasswordHash: "hash",
		Role: service.RoleUser, Status: service.StatusActive,
	})
	_, err := repo.EnsureUserAffiliate(txCtx, uPlain.ID)
	require.NoError(t, err)

	// User with a custom code — should appear.
	uCode := mustCreateUser(t, client, &service.User{
		Email:        fmt.Sprintf("affiliate-codeonly-%d@example.com", time.Now().UnixNano()),
		PasswordHash: "hash",
		Role:         service.RoleUser, Status: service.StatusActive,
	})
	require.NoError(t, repo.UpdateUserAffCode(txCtx, uCode.ID, fmt.Sprintf("VIP%09d", time.Now().UnixNano()%1_000_000_000)))

	// User with only an exclusive rate — should appear.
	uRate := mustCreateUser(t, client, &service.User{
		Email:        fmt.Sprintf("affiliate-rateonly-%d@example.com", time.Now().UnixNano()),
		PasswordHash: "hash",
		Role:         service.RoleUser, Status: service.StatusActive,
	})
	r := 33.3
	require.NoError(t, repo.SetUserRebateRate(txCtx, uRate.ID, &r))

	entries, total, err := repo.ListUsersWithCustomSettings(txCtx, service.AffiliateAdminFilter{
		Page: 1, PageSize: 100,
	})
	require.NoError(t, err)

	// Build a quick lookup to assert per-user attributes (other tests may have
	// inserted custom rows in the same DB; we only care about our 3).
	byUserID := make(map[int64]service.AffiliateAdminEntry, len(entries))
	for _, e := range entries {
		byUserID[e.UserID] = e
	}

	require.NotContains(t, byUserID, uPlain.ID, "users without overrides must not appear")

	codeEntry, ok := byUserID[uCode.ID]
	require.True(t, ok, "custom-code user missing from list")
	require.True(t, codeEntry.AffCodeCustom)
	require.Nil(t, codeEntry.AffRebateRatePercent)

	rateEntry, ok := byUserID[uRate.ID]
	require.True(t, ok, "custom-rate user missing from list")
	require.False(t, rateEntry.AffCodeCustom)
	require.NotNil(t, rateEntry.AffRebateRatePercent)
	require.InDelta(t, 33.3, *rateEntry.AffRebateRatePercent, 1e-9)

	require.GreaterOrEqual(t, total, int64(2), "total must include at least our 2 custom rows")
}
