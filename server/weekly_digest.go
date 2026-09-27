package server

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/usememos/memos/core/digest"
	storepb "github.com/usememos/memos/proto/gen/store"
	"github.com/usememos/memos/store"
)

const (
	weeklyDigestPollInterval     = time.Hour
	weeklyDigestHistoryRetention = 52 * 7 * 24 * time.Hour
)

// WeeklyDigestPeriod identifies one Monday 09:00 UTC reporting window.
type WeeklyDigestPeriod struct {
	Key   string
	Start time.Time
	End   time.Time
}

// WeeklyDigestDelivery hands one rendered digest to the configured sender and
// reports whether a message was actually sent.
// Rendering stays in core/digest; the server owns scheduling and durable
// claims. A delivery callback should return only after the sender has accepted
// the message. SMTP itself cannot provide an exact-once guarantee.
type WeeklyDigestDelivery func(context.Context, *store.User, WeeklyDigestPeriod) (bool, error)

// WeeklyDigestOptIn checks a user's generated general setting. It is an
// injection point so the server does not depend on generated protobuf output;
// callers can wire GeneralUserSetting.WeeklyMemoSummaryEmails after buf
// generation. A nil callback means opt-in is false.
type WeeklyDigestOptIn func(context.Context, *store.User) (bool, error)

// WeeklyDigestRunnerOptions configures a WeeklyDigestRunner. Clock and
// PollInterval are injectable to make period and lifecycle tests deterministic.
type WeeklyDigestRunnerOptions struct {
	Clock        func() time.Time
	PollInterval time.Duration
	OptIn        WeeklyDigestOptIn
	Deliver      WeeklyDigestDelivery
}

// WeeklyDigestRunner polls for the latest completed weekly period and sends
// each eligible normal user once per successful period. Failed attempts are
// released for retry by a later poll.
type WeeklyDigestRunner struct {
	store *store.Store

	mu           sync.RWMutex
	clock        func() time.Time
	pollInterval time.Duration
	optIn        WeeklyDigestOptIn
	deliver      WeeklyDigestDelivery

	startOnce sync.Once
	stopOnce  sync.Once
	started   bool
	stop      chan struct{}
	done      chan struct{}
	runCancel context.CancelFunc
}

// NewWeeklyDigestRunner constructs a server-owned weekly digest runner.
func NewWeeklyDigestRunner(st *store.Store, options WeeklyDigestRunnerOptions) *WeeklyDigestRunner {
	clock := options.Clock
	if clock == nil {
		clock = time.Now
	}
	pollInterval := options.PollInterval
	if pollInterval <= 0 {
		pollInterval = weeklyDigestPollInterval
	}
	optIn := options.OptIn
	if optIn == nil {
		optIn = WeeklyDigestOptInFromStore(st)
	}
	return &WeeklyDigestRunner{
		store:        st,
		clock:        clock,
		pollInterval: pollInterval,
		optIn:        optIn,
		deliver:      options.Deliver,
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
	}
}

// WeeklyDigestOptInFromStore reads the generated general user setting through
// protobuf reflection. Older binaries whose generated descriptor does not yet
// contain weekly_memo_summary_emails safely return false; regenerated outputs
// expose the field without requiring another server change.
func WeeklyDigestOptInFromStore(st *store.Store) WeeklyDigestOptIn {
	return func(ctx context.Context, user *store.User) (bool, error) {
		if st == nil || user == nil {
			return false, nil
		}
		setting, err := st.GetUserSetting(ctx, &store.FindUserSetting{
			UserID: &user.ID,
			Key:    storepb.UserSetting_GENERAL,
		})
		if err != nil {
			return false, err
		}
		if setting == nil || setting.GetGeneral() == nil {
			return false, nil
		}
		general := setting.GetGeneral()
		field := general.ProtoReflect().Descriptor().Fields().ByName("weekly_memo_summary_emails")
		if field == nil || field.Kind() != protoreflect.BoolKind {
			return false, nil
		}
		return general.ProtoReflect().Get(field).Bool(), nil
	}
}

// SetDelivery wires the core digest renderer/sender before Start. It is the
// small integration hook between the server-owned runner and core/digest.
func (r *WeeklyDigestRunner) SetDelivery(optIn WeeklyDigestOptIn, deliver WeeklyDigestDelivery) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if optIn == nil {
		optIn = WeeklyDigestOptInFromStore(r.store)
	}
	r.optIn = optIn
	r.deliver = deliver
}

// Start begins hourly polling. It is safe to call more than once.
func (r *WeeklyDigestRunner) Start() {
	r.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		r.mu.Lock()
		r.started = true
		r.runCancel = cancel
		r.mu.Unlock()
		go r.run(ctx)
	})
}

func (r *WeeklyDigestRunner) run(ctx context.Context) {
	defer close(r.done)
	defer func() {
		r.mu.Lock()
		r.runCancel = nil
		r.mu.Unlock()
	}()

	// Run immediately so a restart does not wait another hour to catch up.
	if err := r.RunOnce(ctx); err != nil {
		slog.Warn("weekly digest run failed", "error", err)
	}
	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := r.RunOnce(ctx); err != nil {
				slog.Warn("weekly digest run failed", "error", err)
			}
		case <-r.stop:
			return
		}
	}
}

// Stop stops polling and waits for an in-flight run before the store can close.
func (r *WeeklyDigestRunner) Stop(ctx context.Context) error {
	r.mu.RLock()
	started := r.started
	r.mu.RUnlock()
	if !started {
		return nil
	}
	r.stopOnce.Do(func() {
		r.mu.RLock()
		cancel := r.runCancel
		r.mu.RUnlock()
		if cancel != nil {
			cancel()
		}
		close(r.stop)
	})
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RunOnce evaluates and delivers the latest completed period. It is exported
// for deterministic tests and for operators that want an explicit catch-up.
func (r *WeeklyDigestRunner) RunOnce(ctx context.Context) error {
	if r.store == nil {
		return errors.New("weekly digest store is required")
	}
	r.mu.RLock()
	optIn, deliver := r.optIn, r.deliver
	clock := r.clock
	r.mu.RUnlock()
	if deliver == nil {
		// Keep the feature inert until core/digest wires its sender. In
		// particular, do not create claims that could suppress a later wiring.
		return nil
	}

	period := WeeklyDigestPeriodFor(clock())
	status := store.Normal
	users, err := r.store.ListUsers(ctx, &store.FindUser{RowStatus: &status})
	if err != nil {
		return errors.Wrap(err, "failed to list weekly digest users")
	}

	var firstErr error
	for _, user := range users {
		if user == nil || strings.TrimSpace(user.Email) == "" {
			continue
		}
		if optIn == nil {
			continue
		}
		ok, err := optIn(ctx, user)
		if err != nil {
			if firstErr == nil {
				firstErr = errors.Wrapf(err, "failed to read weekly digest opt-in for user %d", user.ID)
			}
			continue
		}
		if !ok {
			continue
		}
		claimed, err := r.store.ClaimWeeklyDigestDeliveryAt(ctx, user.ID, period.Key, clock())
		if err != nil {
			if firstErr == nil {
				firstErr = errors.Wrapf(err, "failed to claim weekly digest for user %d", user.ID)
			}
			continue
		}
		if !claimed {
			continue
		}
		sent, err := deliver(ctx, user, period)
		if err != nil {
			releaseCtx := context.WithoutCancel(ctx)
			if releaseErr := r.store.ReleaseWeeklyDigestDelivery(releaseCtx, user.ID, period.Key); releaseErr != nil {
				if firstErr == nil {
					firstErr = errors.Wrapf(releaseErr, "failed to release weekly digest claim for user %d", user.ID)
				}
			}
			if firstErr == nil {
				firstErr = errors.Wrapf(err, "failed to deliver weekly digest for user %d", user.ID)
			}
			continue
		}
		if !sent {
			if err := r.store.ReleaseWeeklyDigestDelivery(context.WithoutCancel(ctx), user.ID, period.Key); err != nil && firstErr == nil {
				firstErr = errors.Wrapf(err, "failed to release skipped weekly digest claim for user %d", user.ID)
			}
			continue
		}
		if err := r.store.MarkWeeklyDigestDelivered(ctx, user.ID, period.Key, clock()); err != nil {
			if firstErr == nil {
				firstErr = errors.Wrapf(err, "failed to mark weekly digest delivered for user %d", user.ID)
			}
		}
	}
	if err := r.store.PruneWeeklyDigestDeliveries(ctx, period.End.Add(-weeklyDigestHistoryRetention)); err != nil && firstErr == nil {
		firstErr = errors.Wrap(err, "failed to prune weekly digest history")
	}
	return firstErr
}

// WeeklyDigestPeriodFor returns the latest period ending at a Monday 09:00 UTC
// boundary. Its key is the UTC date of the period start.
func WeeklyDigestPeriodFor(now time.Time) WeeklyDigestPeriod {
	window := digest.WeeklyWindow(now)
	return WeeklyDigestPeriod{
		Key:   window.Key(),
		Start: window.Start,
		End:   window.End,
	}
}
