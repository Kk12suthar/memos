// Package digest contains the domain logic for weekly memo digest emails.
package digest

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/usememos/memos/core/notification"
	"github.com/usememos/memos/internal/email"
	"github.com/usememos/memos/markdown"
	storepb "github.com/usememos/memos/proto/gen/store"
	"github.com/usememos/memos/store"
)

const (
	// DefaultSnippetLength is the maximum number of characters included from a
	// memo in a digest entry.
	DefaultSnippetLength = 240
	week                 = 7 * 24 * time.Hour
)

// MemoLister is the store capability required to build a digest.
//
// *store.Store satisfies this interface. Keeping the capability narrow makes
// the digest straightforward to exercise without a database and lets a
// runner provide its existing store facade.
type MemoLister interface {
	ListMemos(context.Context, *store.FindMemo) ([]*store.Memo, error)
}

// Sender synchronously sends one prepared digest email. Returning an error is
// intentional: a caller that keeps a durable delivery ledger must only mark a
// period successful after this function returns nil.
type Sender func(context.Context, *email.Config, *email.Message) error

// Option configures a Builder.
type Option func(*Builder)

// WithClock injects the clock used by CurrentWindow. The explicit Window
// passed to Build and Send is always preferred when a runner already owns its
// period calculation.
func WithClock(clock func() time.Time) Option {
	return func(builder *Builder) {
		if clock != nil {
			builder.clock = clock
		}
	}
}

// WithSender injects the synchronous email sender used by Send.
func WithSender(sender Sender) Option {
	return func(builder *Builder) {
		if sender != nil {
			builder.sender = sender
		}
	}
}

// WithSnippetLength sets the maximum length passed to GenerateSnippet.
func WithSnippetLength(length int) Option {
	return func(builder *Builder) {
		if length > 0 {
			builder.snippetLength = length
		}
	}
}

// Window is a half-open UTC period. A memo created at Start is included; a
// memo created at End belongs to the following period.
type Window struct {
	Start time.Time
	End   time.Time
}

// NewWindow constructs a seven-day digest window.
func NewWindow(start, end time.Time) (Window, error) {
	window := Window{Start: start.UTC(), End: end.UTC()}
	if err := window.Validate(); err != nil {
		return Window{}, err
	}
	return window, nil
}

// Validate checks that the window is a seven-day half-open period.
func (w Window) Validate() error {
	if w.Start.IsZero() || w.End.IsZero() {
		return errors.New("digest window must have a start and end")
	}
	if !w.End.After(w.Start) {
		return errors.New("digest window end must be after start")
	}
	if w.End.Sub(w.Start) != week {
		return errors.New("digest window must be seven days")
	}
	return nil
}

// Key returns a stable period key suitable for a delivery ledger.
func (w Window) Key() string {
	return w.Start.UTC().Format(time.RFC3339)
}

// WeeklyWindow returns the most recently completed Monday 09:00 UTC period.
// It is deterministic for a given now value and always spans exactly seven
// days.
func WeeklyWindow(now time.Time) Window {
	now = now.UTC()
	boundary := time.Date(now.Year(), now.Month(), now.Day(), 9, 0, 0, 0, time.UTC)
	daysSinceMonday := (int(boundary.Weekday()) + 6) % 7
	boundary = boundary.Add(-time.Duration(daysSinceMonday) * 24 * time.Hour)
	if now.Before(boundary) {
		boundary = boundary.Add(-week)
	}
	return Window{Start: boundary.Add(-week), End: boundary}
}

// Entry is one memo in a digest.
type Entry struct {
	UID       string
	CreatedAt time.Time
	Snippet   string
	URL       string
}

// Digest is the rendered result for one user and one period. Message is nil
// when the user has no qualifying memos, so callers can skip an empty email.
type Digest struct {
	UserID  int32
	Window  Window
	Entries []Entry
	Message *email.Message
}

// Builder builds and sends weekly digest emails.
type Builder struct {
	Memos           MemoLister
	MarkdownService markdown.Service
	InstanceURL     string

	clock         func() time.Time
	sender        Sender
	snippetLength int
}

// NewBuilder creates a digest builder. The default sender is synchronous so a
// successful return means the email send completed successfully.
func NewBuilder(memos MemoLister, markdownService markdown.Service, instanceURL string, opts ...Option) *Builder {
	builder := &Builder{
		Memos:           memos,
		MarkdownService: markdownService,
		InstanceURL:     strings.TrimRight(strings.TrimSpace(instanceURL), "/"),
		clock:           time.Now,
		sender: func(ctx context.Context, config *email.Config, message *email.Message) error {
			return email.SendContext(ctx, config, message)
		},
		snippetLength: DefaultSnippetLength,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(builder)
		}
	}
	return builder
}

// NewService is an alias for NewBuilder for callers that use service naming
// for their domain dependencies.
func NewService(memos MemoLister, markdownService markdown.Service, instanceURL string, opts ...Option) *Builder {
	return NewBuilder(memos, markdownService, instanceURL, opts...)
}

// CurrentWindow returns the most recently completed weekly period according
// to the injected clock.
func (b *Builder) CurrentWindow() Window {
	return WeeklyWindow(b.clock())
}

// Build creates the digest for user in window. It asks the store for the
// user's memos with comments excluded and no row-status restriction, then
// applies the half-open time window in domain code. The latter keeps the
// boundary semantics explicit and protects callers using a lightweight store
// implementation in tests.
func (b *Builder) Build(ctx context.Context, user *store.User, window Window) (*Digest, error) {
	if user == nil {
		return nil, errors.New("digest user is required")
	}
	window = Window{Start: window.Start.UTC(), End: window.End.UTC()}
	if err := window.Validate(); err != nil {
		return nil, errors.Wrap(err, "invalid digest window")
	}
	if b.Memos == nil {
		return nil, errors.New("digest memo store is required")
	}
	if b.MarkdownService == nil {
		return nil, errors.New("digest markdown service is required")
	}

	creatorID := user.ID
	memos, err := b.Memos.ListMemos(ctx, &store.FindMemo{
		CreatorID:       &creatorID,
		ExcludeComments: true,
		Filters: []string{fmt.Sprintf(
			`created_ts >= timestamp(%d) && created_ts < timestamp(%d)`,
			window.Start.Unix(), window.End.Unix(),
		)},
		OrderByTimeAsc: true,
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to list digest memos")
	}

	// Store drivers order by created_ts when requested. Sorting again makes the
	// output deterministic for alternate MemoLister implementations.
	sort.SliceStable(memos, func(i, j int) bool {
		if memos[i] == nil {
			return false
		}
		if memos[j] == nil {
			return true
		}
		if memos[i].CreatedTs != memos[j].CreatedTs {
			return memos[i].CreatedTs < memos[j].CreatedTs
		}
		return memos[i].ID < memos[j].ID
	})

	digest := &Digest{UserID: user.ID, Window: window, Entries: make([]Entry, 0, len(memos))}
	for _, memo := range memos {
		if memo == nil || memo.CreatorID != user.ID {
			continue
		}
		// ParentUID is populated for comment memos. The SQL predicate above is
		// authoritative for real stores; this check keeps the domain invariant
		// true if a store implementation returns a broader result.
		if memo.ParentUID != nil {
			continue
		}
		createdAt := time.Unix(memo.CreatedTs, 0).UTC()
		if createdAt.Before(window.Start) || !createdAt.Before(window.End) {
			continue
		}
		snippet, err := b.MarkdownService.GenerateSnippet([]byte(memo.Content), b.snippetLength)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to generate digest snippet for memo %d", memo.ID)
		}
		digest.Entries = append(digest.Entries, Entry{
			UID:       memo.UID,
			CreatedAt: createdAt,
			Snippet:   snippet,
			URL:       b.memoURL(memo.UID),
		})
	}

	if len(digest.Entries) == 0 {
		return digest, nil
	}
	digest.Message = &email.Message{
		To:      []string{strings.TrimSpace(user.Email)},
		Subject: fmt.Sprintf("[Memos] Weekly digest: %s", window.Start.Format("2006-01-02")),
		Body:    renderBody(window, digest.Entries),
	}
	return digest, nil
}

// Send builds and synchronously sends the digest. The returned boolean is true
// only when a message was handed to the sender. A disabled or missing email
// setting, missing instance URL, empty recipient, or empty digest is a no-op,
// matching other notification dispatchers. Sender errors are returned to the
// caller and must prevent a durable success mark.
func (b *Builder) Send(ctx context.Context, user *store.User, window Window, setting *storepb.InstanceNotificationSetting_EmailSetting) (bool, error) {
	if setting == nil || !setting.GetEnabled() || b.InstanceURL == "" || user == nil || strings.TrimSpace(user.Email) == "" {
		return false, nil
	}
	digest, err := b.Build(ctx, user, window)
	if err != nil {
		return false, err
	}
	if digest.Message == nil {
		return false, nil
	}
	if len(digest.Message.To) == 0 || strings.TrimSpace(digest.Message.To[0]) == "" {
		return false, nil
	}
	digest.Message.ReplyTo = setting.GetReplyTo()
	config := notification.EmailConfigFromInstanceSetting(setting)
	if err := config.Validate(); err != nil {
		return false, errors.Wrap(err, "invalid digest email setting")
	}
	sender := b.sender
	if sender == nil {
		sender = func(ctx context.Context, config *email.Config, message *email.Message) error {
			return email.SendContext(ctx, config, message)
		}
	}
	if err := sender(ctx, config, digest.Message); err != nil {
		return false, errors.Wrap(err, "failed to send weekly digest")
	}
	return true, nil
}

func (b *Builder) memoURL(uid string) string {
	if b.InstanceURL == "" || uid == "" {
		return ""
	}
	return fmt.Sprintf("%s/memos/%s", b.InstanceURL, uid)
}

func renderBody(window Window, entries []Entry) string {
	lines := []string{
		"Weekly digest",
		fmt.Sprintf("Period: %s – %s", window.Start.Format("2006-01-02 15:04 UTC"), window.End.Format("2006-01-02 15:04 UTC")),
		"",
	}
	for index, entry := range entries {
		lines = append(lines, fmt.Sprintf("%d. %s", index+1, entry.CreatedAt.Format("2006-01-02 15:04 UTC")))
		if entry.Snippet != "" {
			lines = append(lines, entry.Snippet)
		}
		if entry.URL != "" {
			lines = append(lines, entry.URL)
		}
		if index < len(entries)-1 {
			lines = append(lines, "")
		}
	}
	return strings.Join(lines, "\n")
}
