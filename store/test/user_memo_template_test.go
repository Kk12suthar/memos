package test

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	storepb "github.com/usememos/memos/proto/gen/store"
)

func TestUserMemoTemplateCRUD(t *testing.T) {
	ctx := context.Background()
	ts := NewTestingStore(ctx, t)
	defer ts.Close()
	owner, err := createTestingHostUser(ctx, ts)
	require.NoError(t, err)

	removed, err := ts.RemoveUserMemoTemplate(ctx, owner.ID, "missing")
	require.NoError(t, err)
	require.False(t, removed)

	original := &storepb.MemoTemplatesUserSetting_MemoTemplate{Id: "daily", Title: "Daily log", Content: "# Daily log\n\n- "}
	require.NoError(t, ts.AddUserMemoTemplate(ctx, owner.ID, original))

	templates, err := ts.GetUserMemoTemplates(ctx, owner.ID)
	require.NoError(t, err)
	require.Len(t, templates, 1)
	require.Equal(t, original.Title, templates[0].Title)

	updated, err := ts.UpdateUserMemoTemplate(ctx, owner.ID, original.Id, new("Updated log"), new("## Updated\n"))
	require.NoError(t, err)
	require.Equal(t, "Updated log", updated.Title)
	require.Equal(t, "## Updated\n", updated.Content)

	removed, err = ts.RemoveUserMemoTemplate(ctx, owner.ID, original.Id)
	require.NoError(t, err)
	require.True(t, removed)
	removed, err = ts.RemoveUserMemoTemplate(ctx, owner.ID, original.Id)
	require.NoError(t, err)
	require.False(t, removed)
}

func TestUserMemoTemplateConcurrentWrites(t *testing.T) {
	ctx := context.Background()
	ts := NewTestingStore(ctx, t)
	defer ts.Close()
	owner, err := createTestingHostUser(ctx, ts)
	require.NoError(t, err)

	const concurrency = 8
	start := make(chan struct{})
	errs := make([]error, concurrency)
	var wg sync.WaitGroup
	for i := range concurrency {
		wg.Go(func() {
			<-start
			errs[i] = ts.AddUserMemoTemplate(ctx, owner.ID, &storepb.MemoTemplatesUserSetting_MemoTemplate{
				Id:      string(rune('a' + i)),
				Title:   "Template",
				Content: "content",
			})
		})
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}

	templates, err := ts.GetUserMemoTemplates(ctx, owner.ID)
	require.NoError(t, err)
	require.Len(t, templates, concurrency)
}
