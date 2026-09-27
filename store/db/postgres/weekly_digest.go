package postgres

import (
	"context"
	"database/sql"
	"strings"

	"github.com/usememos/memos/store"
)

func (d *DB) ClaimWeeklyDigestDelivery(ctx context.Context, claim *store.WeeklyDigestDelivery) (bool, error) {
	result, err := d.db.ExecContext(ctx, `
		INSERT INTO weekly_digest_delivery (user_id, period_key, claimed_ts)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, period_key) DO UPDATE SET
			claimed_ts = EXCLUDED.claimed_ts,
			delivered_ts = NULL
		WHERE weekly_digest_delivery.delivered_ts IS NULL
			AND weekly_digest_delivery.claimed_ts <= $4
	`, claim.UserID, claim.PeriodKey, claim.ClaimedTs, claim.StaleBeforeTs)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func (d *DB) MarkWeeklyDigestDelivered(ctx context.Context, delivery *store.WeeklyDigestDelivery) error {
	result, err := d.db.ExecContext(ctx, `
		UPDATE weekly_digest_delivery
		SET delivered_ts = $1
		WHERE user_id = $2 AND period_key = $3 AND delivered_ts IS NULL
	`, delivery.DeliveredTs, delivery.UserID, delivery.PeriodKey)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return store.ErrWeeklyDigestDeliveryNotFound
	}
	return nil
}

func (d *DB) ReleaseWeeklyDigestDelivery(ctx context.Context, delivery *store.WeeklyDigestDelivery) error {
	_, err := d.db.ExecContext(ctx, `
		DELETE FROM weekly_digest_delivery
		WHERE user_id = $1 AND period_key = $2 AND delivered_ts IS NULL
	`, delivery.UserID, delivery.PeriodKey)
	return err
}

func (d *DB) PruneWeeklyDigestDeliveries(ctx context.Context, beforeUnix int64) error {
	_, err := d.db.ExecContext(ctx, `DELETE FROM weekly_digest_delivery WHERE claimed_ts < $1`, beforeUnix)
	return err
}

func (d *DB) ListWeeklyDigestDeliveries(ctx context.Context, find *store.FindWeeklyDigestDelivery) ([]*store.WeeklyDigestDelivery, error) {
	where, args := []string{"1 = 1"}, []any{}
	if find.UserID != nil {
		where = append(where, "user_id = "+placeholder(len(args)+1))
		args = append(args, *find.UserID)
	}
	if find.PeriodKey != nil {
		where = append(where, "period_key = "+placeholder(len(args)+1))
		args = append(args, *find.PeriodKey)
	}
	rows, err := d.db.QueryContext(ctx, "SELECT user_id, period_key, claimed_ts, delivered_ts FROM weekly_digest_delivery WHERE "+strings.Join(where, " AND ")+" ORDER BY user_id, period_key", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	list := make([]*store.WeeklyDigestDelivery, 0)
	for rows.Next() {
		row := &store.WeeklyDigestDelivery{}
		var delivered sql.NullInt64
		if err := rows.Scan(&row.UserID, &row.PeriodKey, &row.ClaimedTs, &delivered); err != nil {
			return nil, err
		}
		if delivered.Valid {
			row.DeliveredTs = &delivered.Int64
		}
		list = append(list, row)
	}
	return list, rows.Err()
}
