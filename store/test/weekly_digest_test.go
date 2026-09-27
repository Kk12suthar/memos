package test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/store"
)

func TestWeeklyDigestDeliveryClaimIsUnique(t *testing.T) {
	ctx := context.Background()
	ts := NewTestingStore(ctx, t)
	defer ts.Close()

	user, err := ts.CreateUser(ctx, &store.User{
		Username:     "weekly-digest-user",
		Role:         store.RoleUser,
		Email:        "weekly-digest@example.com",
		PasswordHash: "hash",
	})
	require.NoError(t, err)

	claimed, err := ts.ClaimWeeklyDigestDelivery(ctx, user.ID, "2026-09-14")
	require.NoError(t, err)
	require.True(t, claimed)

	claimed, err = ts.ClaimWeeklyDigestDelivery(ctx, user.ID, "2026-09-14")
	require.NoError(t, err)
	require.False(t, claimed)

	deliveredAt := int64(123)
	require.NoError(t, ts.MarkWeeklyDigestDelivered(ctx, user.ID, "2026-09-14", time.Unix(deliveredAt, 0)))
	rows, err := ts.ListWeeklyDigestDeliveries(ctx, &store.FindWeeklyDigestDelivery{UserID: &user.ID})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, deliveredAt, *rows[0].DeliveredTs)
}
