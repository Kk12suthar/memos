package store

import (
	"context"
	"strings"
	"time"

	"github.com/pkg/errors"
)

// WeeklyDigestClaimLease is the amount of time an unfinished delivery claim
// remains owned by a worker before another run may recover it.
const WeeklyDigestClaimLease = 2 * time.Hour

// ErrWeeklyDigestDeliveryNotFound indicates that a delivery could not be
// marked because its uncompleted claim no longer exists.
var ErrWeeklyDigestDeliveryNotFound = errors.New("weekly digest delivery claim not found")

// WeeklyDigestDelivery records the durable claim for one user's digest period.
// ClaimedTs is written when a worker claims the period; DeliveredTs is written
// after the sender accepts the message. Unsuccessful claims are released so a
// later run can retry; a successful handoff is not exact-once SMTP delivery.
type WeeklyDigestDelivery struct {
	UserID        int32
	PeriodKey     string
	ClaimedTs     int64
	StaleBeforeTs int64
	DeliveredTs   *int64
}

// FindWeeklyDigestDelivery identifies ledger rows to read.
type FindWeeklyDigestDelivery struct {
	UserID    *int32
	PeriodKey *string
}

// ClaimWeeklyDigestDelivery atomically claims a user/period pair. It returns
// true only for the worker that inserted a new row or recovered an expired
// unfinished claim.
func (s *Store) ClaimWeeklyDigestDelivery(ctx context.Context, userID int32, periodKey string) (bool, error) {
	return s.ClaimWeeklyDigestDeliveryAt(ctx, userID, periodKey, time.Now())
}

// ClaimWeeklyDigestDeliveryAt is the clock-injectable form of
// ClaimWeeklyDigestDelivery.
func (s *Store) ClaimWeeklyDigestDeliveryAt(ctx context.Context, userID int32, periodKey string, claimedAt time.Time) (bool, error) {
	periodKey = strings.TrimSpace(periodKey)
	if userID <= 0 {
		return false, errors.New("weekly digest user id must be positive")
	}
	if periodKey == "" {
		return false, errors.New("weekly digest period key is required")
	}
	if claimedAt.IsZero() {
		return false, errors.New("weekly digest claim time is required")
	}
	claimedUnix := claimedAt.Unix()
	return s.driver.ClaimWeeklyDigestDelivery(ctx, &WeeklyDigestDelivery{
		UserID:        userID,
		PeriodKey:     periodKey,
		ClaimedTs:     claimedUnix,
		StaleBeforeTs: claimedAt.Add(-WeeklyDigestClaimLease).Unix(),
	})
}

// MarkWeeklyDigestDelivered records a successful handoff to the email sender.
func (s *Store) MarkWeeklyDigestDelivered(ctx context.Context, userID int32, periodKey string, deliveredTs time.Time) error {
	periodKey = strings.TrimSpace(periodKey)
	if userID <= 0 {
		return errors.New("weekly digest user id must be positive")
	}
	if periodKey == "" {
		return errors.New("weekly digest period key is required")
	}
	if deliveredTs.IsZero() {
		deliveredTs = time.Now()
	}
	deliveredUnix := deliveredTs.Unix()
	return s.driver.MarkWeeklyDigestDelivered(ctx, &WeeklyDigestDelivery{
		UserID:      userID,
		PeriodKey:   periodKey,
		DeliveredTs: &deliveredUnix,
	})
}

// ReleaseWeeklyDigestDelivery removes an unsuccessful claim so a later run can
// retry the period. Delivered rows are never released.
func (s *Store) ReleaseWeeklyDigestDelivery(ctx context.Context, userID int32, periodKey string) error {
	periodKey = strings.TrimSpace(periodKey)
	if userID <= 0 {
		return errors.New("weekly digest user id must be positive")
	}
	if periodKey == "" {
		return errors.New("weekly digest period key is required")
	}
	return s.driver.ReleaseWeeklyDigestDelivery(ctx, &WeeklyDigestDelivery{UserID: userID, PeriodKey: periodKey})
}

// PruneWeeklyDigestDeliveries removes old delivery history and unfinished
// claims. The claim lease handles active recovery; retention bounds ledger
// growth over the lifetime of the instance.
func (s *Store) PruneWeeklyDigestDeliveries(ctx context.Context, before time.Time) error {
	if before.IsZero() {
		return errors.New("weekly digest prune time is required")
	}
	return s.driver.PruneWeeklyDigestDeliveries(ctx, before.Unix())
}

// ListWeeklyDigestDeliveries returns durable weekly digest ledger rows.
func (s *Store) ListWeeklyDigestDeliveries(ctx context.Context, find *FindWeeklyDigestDelivery) ([]*WeeklyDigestDelivery, error) {
	if find == nil {
		find = &FindWeeklyDigestDelivery{}
	}
	return s.driver.ListWeeklyDigestDeliveries(ctx, find)
}
