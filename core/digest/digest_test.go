package digest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/internal/email"
	"github.com/usememos/memos/markdown"
	storepb "github.com/usememos/memos/proto/gen/store"
	"github.com/usememos/memos/store"
)

type fakeMemoLister struct {
	memos []*store.Memo
	find  *store.FindMemo
}

func (f *fakeMemoLister) ListMemos(_ context.Context, find *store.FindMemo) ([]*store.Memo, error) {
	f.find = find
	return f.memos, nil
}

func TestBuildFiltersOwnerCommentsAndHalfOpenWindow(t *testing.T) {
	start := time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC)
	end := start.Add(week)
	owner := &store.User{ID: 7, Email: "owner@example.com"}
	commentParent := "parent"
	lister := &fakeMemoLister{memos: []*store.Memo{
		{ID: 1, UID: "at-start", CreatorID: owner.ID, CreatedTs: start.Unix(), Content: "# At start\nIncluded", RowStatus: store.Archived},
		{ID: 2, UID: "at-end", CreatorID: owner.ID, CreatedTs: end.Unix(), Content: "Excluded at end"},
		{ID: 3, UID: "before", CreatorID: owner.ID, CreatedTs: start.Add(-time.Second).Unix(), Content: "Excluded before"},
		{ID: 4, UID: "after", CreatorID: owner.ID, CreatedTs: end.Add(time.Second).Unix(), Content: "Excluded after"},
		{ID: 5, UID: "other-owner", CreatorID: 8, CreatedTs: start.Add(time.Hour).Unix(), Content: "Excluded owner"},
		{ID: 6, UID: "comment", CreatorID: owner.ID, CreatedTs: start.Add(2 * time.Hour).Unix(), Content: "Excluded comment", ParentUID: &commentParent},
		{ID: 7, UID: "second", CreatorID: owner.ID, CreatedTs: start.Add(time.Hour).Unix(), Content: "A **second** memo"},
	}}
	builder := NewBuilder(lister, markdown.NewService(), "https://memos.example/", WithSnippetLength(80))

	digest, err := builder.Build(context.Background(), owner, Window{Start: start, End: end})
	require.NoError(t, err)
	require.Len(t, digest.Entries, 2)
	require.Equal(t, "at-start", digest.Entries[0].UID)
	require.Equal(t, "second", digest.Entries[1].UID)
	var archivedIncluded bool
	for _, entry := range digest.Entries {
		if entry.UID == "at-start" {
			archivedIncluded = true
		}
	}
	require.True(t, archivedIncluded, "archived memos remain eligible")
	require.Contains(t, digest.Entries[0].Snippet, "At start")
	require.Equal(t, "https://memos.example/memos/at-start", digest.Entries[0].URL)
	require.Contains(t, digest.Message.Body, "A second memo")

	require.NotNil(t, lister.find)
	require.NotNil(t, lister.find.CreatorID)
	require.Equal(t, owner.ID, *lister.find.CreatorID)
	require.True(t, lister.find.ExcludeComments)
	require.True(t, lister.find.OrderByTimeAsc)
	require.Nil(t, lister.find.RowStatus, "the query must include archived rows")
}

func TestWeeklyWindowUsesMondayNineUTC(t *testing.T) {
	tests := []struct {
		name string
		now  time.Time
		end  time.Time
	}{
		{
			name: "before boundary",
			now:  time.Date(2026, 9, 21, 8, 59, 0, 0, time.UTC),
			end:  time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC),
		},
		{
			name: "at boundary",
			now:  time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC),
			end:  time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC),
		},
		{
			name: "midweek",
			now:  time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC),
			end:  time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			window := WeeklyWindow(tt.now)
			require.Equal(t, tt.end, window.End)
			require.Equal(t, week, window.End.Sub(window.Start))
			require.NoError(t, window.Validate())
		})
	}
}

func TestSendReturnsSenderErrorAndUsesSMTPConfig(t *testing.T) {
	owner := &store.User{ID: 7, Email: "owner@example.com"}
	start := time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC)
	lister := &fakeMemoLister{memos: []*store.Memo{{
		ID: 1, UID: "memo", CreatorID: owner.ID, CreatedTs: start.Add(time.Hour).Unix(), Content: "Digest content",
	}}}
	senderErr := errors.New("SMTP unavailable")
	var gotConfig *email.Config
	var gotMessage *email.Message
	builder := NewBuilder(lister, markdown.NewService(), "https://memos.example",
		WithSender(func(_ context.Context, config *email.Config, message *email.Message) error {
			gotConfig = config
			gotMessage = message
			return senderErr
		}),
	)
	setting := &storepb.InstanceNotificationSetting_EmailSetting{
		Enabled:   true,
		SmtpHost:  "smtp.example.com",
		SmtpPort:  587,
		FromEmail: "memos@example.com",
		ReplyTo:   "reply@example.com",
		UseTls:    true,
	}

	sent, err := builder.Send(context.Background(), owner, Window{Start: start, End: start.Add(week)}, setting)
	require.False(t, sent)
	require.ErrorIs(t, err, senderErr)
	require.NotNil(t, gotConfig)
	require.Equal(t, "smtp.example.com", gotConfig.SMTPHost)
	require.Equal(t, 587, gotConfig.SMTPPort)
	require.True(t, gotConfig.UseTLS)
	require.NotNil(t, gotMessage)
	require.Equal(t, "reply@example.com", gotMessage.ReplyTo)
}

func TestSendSkipsEmptyDigest(t *testing.T) {
	owner := &store.User{ID: 7, Email: "owner@example.com"}
	start := time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC)
	sent := false
	builder := NewBuilder(&fakeMemoLister{}, markdown.NewService(), "https://memos.example",
		WithSender(func(_ context.Context, _ *email.Config, _ *email.Message) error {
			sent = true
			return nil
		}),
	)
	setting := &storepb.InstanceNotificationSetting_EmailSetting{Enabled: true, SmtpHost: "smtp.example.com", SmtpPort: 25, FromEmail: "memos@example.com"}

	sent, err := builder.Send(context.Background(), owner, Window{Start: start, End: start.Add(week)}, setting)
	require.NoError(t, err)
	require.False(t, sent)
	require.False(t, sent)
}

func TestSendSkipsWhenInstanceURLIsMissing(t *testing.T) {
	owner := &store.User{ID: 7, Email: "owner@example.com"}
	start := time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC)
	lister := &fakeMemoLister{memos: []*store.Memo{{
		ID: 1, UID: "memo", CreatorID: owner.ID, CreatedTs: start.Add(time.Hour).Unix(), Content: "Digest content",
	}}}
	sent := false
	builder := NewBuilder(lister, markdown.NewService(), "", WithSender(func(_ context.Context, _ *email.Config, _ *email.Message) error {
		sent = true
		return nil
	}))
	setting := &storepb.InstanceNotificationSetting_EmailSetting{Enabled: true, SmtpHost: "smtp.example.com", SmtpPort: 25, FromEmail: "memos@example.com"}

	wasSent, err := builder.Send(context.Background(), owner, Window{Start: start, End: start.Add(week)}, setting)
	require.NoError(t, err)
	require.False(t, wasSent)
	require.False(t, sent)
}
