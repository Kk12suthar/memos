package server

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/internal/profile"
	"github.com/usememos/memos/internal/version"
	storepb "github.com/usememos/memos/proto/gen/store"
	"github.com/usememos/memos/store"
	"github.com/usememos/memos/store/db"
)

func TestWeeklyDigestPeriodForMondayBoundary(t *testing.T) {
	before := WeeklyDigestPeriodFor(time.Date(2026, time.September, 21, 8, 59, 0, 0, time.UTC))
	require.Equal(t, "2026-09-07T09:00:00Z", before.Key)
	require.Equal(t, time.Date(2026, time.September, 7, 9, 0, 0, 0, time.UTC), before.Start)
	require.Equal(t, time.Date(2026, time.September, 14, 9, 0, 0, 0, time.UTC), before.End)

	atBoundary := WeeklyDigestPeriodFor(time.Date(2026, time.September, 21, 9, 0, 0, 0, time.UTC))
	require.Equal(t, "2026-09-14T09:00:00Z", atBoundary.Key)
	require.Equal(t, time.Date(2026, time.September, 14, 9, 0, 0, 0, time.UTC), atBoundary.Start)
	require.Equal(t, time.Date(2026, time.September, 21, 9, 0, 0, 0, time.UTC), atBoundary.End)
}

func TestWeeklyDigestRunnerClaimsEligibleNormalUsers(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	p := &profile.Profile{
		Data:    dataDir,
		DSN:     filepath.Join(dataDir, "memos.db"),
		Driver:  "sqlite",
		Version: version.GetCurrentVersion(),
	}
	driver, err := db.NewDBDriver(p)
	require.NoError(t, err)
	st := store.New(driver, p)
	require.NoError(t, st.Migrate(ctx))
	defer st.Close()

	normalUser, err := st.CreateUser(ctx, &store.User{Username: "digest-normal", Role: store.RoleUser, Email: "normal@example.com", PasswordHash: "hash"})
	require.NoError(t, err)
	enabled := true
	_, err = st.UpsertUserSetting(ctx, &storepb.UserSetting{
		UserId: normalUser.ID,
		Key:    storepb.UserSetting_GENERAL,
		Value:  &storepb.UserSetting_General{General: &storepb.GeneralUserSetting{WeeklyMemoSummaryEmails: &enabled}},
	})
	require.NoError(t, err)
	_, err = st.CreateUser(ctx, &store.User{Username: "digest-admin", Role: store.RoleAdmin, Email: "admin@example.com", PasswordHash: "hash"})
	require.NoError(t, err)
	archivedUser, err := st.CreateUser(ctx, &store.User{Username: "digest-archived", Role: store.RoleUser, Email: "archived@example.com", PasswordHash: "hash"})
	require.NoError(t, err)
	archived := store.Archived
	_, err = st.UpdateUser(ctx, &store.UpdateUser{ID: archivedUser.ID, RowStatus: &archived})
	require.NoError(t, err)

	var delivered atomic.Int32
	runner := NewWeeklyDigestRunner(st, WeeklyDigestRunnerOptions{
		Clock: func() time.Time { return time.Date(2026, time.September, 21, 10, 0, 0, 0, time.UTC) },
		Deliver: func(_ context.Context, user *store.User, period WeeklyDigestPeriod) (bool, error) {
			require.Equal(t, "digest-normal", user.Username)
			require.Equal(t, "2026-09-14T09:00:00Z", period.Key)
			delivered.Add(1)
			return true, nil
		},
	})
	require.NoError(t, runner.RunOnce(ctx))
	require.NoError(t, runner.RunOnce(ctx))
	require.Equal(t, int32(1), delivered.Load())
}

func TestWeeklyDigestClaimRecoversAfterLeaseAndNotAfterDelivery(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	p := &profile.Profile{
		Data:    dataDir,
		DSN:     filepath.Join(dataDir, "memos.db"),
		Driver:  "sqlite",
		Version: version.GetCurrentVersion(),
	}
	driver, err := db.NewDBDriver(p)
	require.NoError(t, err)
	st := store.New(driver, p)
	require.NoError(t, st.Migrate(ctx))
	defer st.Close()

	user, err := st.CreateUser(ctx, &store.User{Username: "digest-lease-user", Role: store.RoleUser, Email: "lease@example.com", PasswordHash: "hash"})
	require.NoError(t, err)
	claimAt := time.Date(2026, time.September, 21, 10, 0, 0, 0, time.UTC)

	claimed, err := st.ClaimWeeklyDigestDeliveryAt(ctx, user.ID, "2026-09-14T09:00:00Z", claimAt)
	require.NoError(t, err)
	require.True(t, claimed)

	claimed, err = st.ClaimWeeklyDigestDeliveryAt(ctx, user.ID, "2026-09-14T09:00:00Z", claimAt.Add(30*time.Minute))
	require.NoError(t, err)
	require.False(t, claimed)

	claimed, err = st.ClaimWeeklyDigestDeliveryAt(ctx, user.ID, "2026-09-14T09:00:00Z", claimAt.Add(store.WeeklyDigestClaimLease+time.Second))
	require.NoError(t, err)
	require.True(t, claimed)

	require.NoError(t, st.MarkWeeklyDigestDelivered(ctx, user.ID, "2026-09-14T09:00:00Z", claimAt.Add(store.WeeklyDigestClaimLease+2*time.Second)))
	claimed, err = st.ClaimWeeklyDigestDeliveryAt(ctx, user.ID, "2026-09-14T09:00:00Z", claimAt.Add(2*store.WeeklyDigestClaimLease))
	require.NoError(t, err)
	require.False(t, claimed)
}
