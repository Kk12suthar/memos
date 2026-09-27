package test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	v1pb "github.com/usememos/memos/proto/gen/api/v1"
)

func TestMemoTemplateCRUD(t *testing.T) {
	ctx := context.Background()
	ts := NewTestService(t)
	defer ts.Cleanup()

	user, err := ts.CreateRegularUser(ctx, "template-user")
	require.NoError(t, err)
	userCtx := ts.CreateUserContext(ctx, user.ID)
	parent := fmt.Sprintf("users/%s", user.Username)

	listed, err := ts.Service.ListMemoTemplates(userCtx, &v1pb.ListMemoTemplatesRequest{Parent: parent})
	require.NoError(t, err)
	require.Empty(t, listed.MemoTemplates)

	created, err := ts.Service.CreateMemoTemplate(userCtx, &v1pb.CreateMemoTemplateRequest{
		Parent: parent,
		MemoTemplate: &v1pb.MemoTemplate{
			Title:   "Daily log",
			Content: "# Daily log\n\n- ",
		},
	})
	require.NoError(t, err)
	require.Equal(t, "Daily log", created.Title)
	require.Equal(t, "# Daily log\n\n- ", created.Content)
	require.Contains(t, created.Name, "/templates/")

	got, err := ts.Service.GetMemoTemplate(userCtx, &v1pb.GetMemoTemplateRequest{Name: created.Name})
	require.NoError(t, err)
	require.Equal(t, created.Name, got.Name)

	updated, err := ts.Service.UpdateMemoTemplate(userCtx, &v1pb.UpdateMemoTemplateRequest{
		MemoTemplate: &v1pb.MemoTemplate{Name: created.Name, Title: "Meeting notes", Content: "# Meeting notes\n"},
		UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"title", "content"}},
	})
	require.NoError(t, err)
	require.Equal(t, "Meeting notes", updated.Title)
	require.Equal(t, "# Meeting notes\n", updated.Content)

	_, err = ts.Service.DeleteMemoTemplate(userCtx, &v1pb.DeleteMemoTemplateRequest{Name: created.Name})
	require.NoError(t, err)
	_, err = ts.Service.GetMemoTemplate(userCtx, &v1pb.GetMemoTemplateRequest{Name: created.Name})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestMemoTemplateValidationAndOwnership(t *testing.T) {
	ctx := context.Background()
	ts := NewTestService(t)
	defer ts.Cleanup()

	owner, err := ts.CreateRegularUser(ctx, "template-owner")
	require.NoError(t, err)
	other, err := ts.CreateRegularUser(ctx, "template-other")
	require.NoError(t, err)
	ownerCtx := ts.CreateUserContext(ctx, owner.ID)
	otherCtx := ts.CreateUserContext(ctx, other.ID)

	_, err = ts.Service.CreateMemoTemplate(ownerCtx, &v1pb.CreateMemoTemplateRequest{
		Parent:       fmt.Sprintf("users/%s", owner.Username),
		MemoTemplate: &v1pb.MemoTemplate{Title: "", Content: "content"},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "title is required")

	created, err := ts.Service.CreateMemoTemplate(ownerCtx, &v1pb.CreateMemoTemplateRequest{
		Parent:       fmt.Sprintf("users/%s", owner.Username),
		MemoTemplate: &v1pb.MemoTemplate{Title: "Private", Content: "content"},
	})
	require.NoError(t, err)

	_, err = ts.Service.GetMemoTemplate(otherCtx, &v1pb.GetMemoTemplateRequest{Name: created.Name})
	require.Error(t, err)
	require.Contains(t, err.Error(), "permission denied")
}
