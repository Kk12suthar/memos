package v1

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/pkg/errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/usememos/memos/internal/random"
	"github.com/usememos/memos/internal/ratelimit"
	v1pb "github.com/usememos/memos/proto/gen/api/v1"
	storepb "github.com/usememos/memos/proto/gen/store"
	"github.com/usememos/memos/server/auth"
	"github.com/usememos/memos/store"
)

const (
	maxMemoTemplateTitleRunes   = 100
	maxMemoTemplateContentBytes = 100 * 1024
)

// extractUserAndMemoTemplateIDFromName extracts the owner and template ID from
// a memo template resource name: users/{user}/templates/{template}.
func (s *APIV1Service) extractUserAndMemoTemplateIDFromName(ctx context.Context, name string) (*store.User, string, error) {
	parts := strings.Split(name, "/")
	if len(parts) != 4 || parts[0] != "users" || parts[2] != "templates" {
		return nil, "", errors.Errorf("invalid memo template name format: %s", name)
	}

	user, err := ResolveUserByName(ctx, s.Store, BuildUserName(parts[1]))
	if err != nil {
		return nil, "", err
	}
	if user == nil {
		return nil, "", errors.Errorf("user not found: %s", parts[1])
	}

	memoTemplateID := parts[3]
	if memoTemplateID == "" {
		return nil, "", errors.Errorf("empty memo template ID in name: %s", name)
	}

	return user, memoTemplateID, nil
}

func constructMemoTemplateName(username string, memoTemplateID string) string {
	return fmt.Sprintf("%s/templates/%s", BuildUserName(username), memoTemplateID)
}

func convertMemoTemplateFromStore(username string, memoTemplate *storepb.MemoTemplatesUserSetting_MemoTemplate) *v1pb.MemoTemplate {
	return &v1pb.MemoTemplate{
		Name:    constructMemoTemplateName(username, memoTemplate.GetId()),
		Title:   memoTemplate.GetTitle(),
		Content: memoTemplate.GetContent(),
	}
}

func validateMemoTemplate(title string, content string) (string, error) {
	title, err := validateMemoTemplateTitle(title)
	if err != nil {
		return "", err
	}
	if err := validateMemoTemplateContent(content); err != nil {
		return "", err
	}
	return title, nil
}

func validateMemoTemplateTitle(title string) (string, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return "", errors.New("title is required")
	}
	if utf8.RuneCountInString(title) > maxMemoTemplateTitleRunes {
		return "", errors.Errorf("title must be at most %d characters", maxMemoTemplateTitleRunes)
	}
	return title, nil
}

func validateMemoTemplateContent(content string) error {
	if strings.TrimSpace(content) == "" {
		return errors.New("content is required")
	}
	if len(content) > maxMemoTemplateContentBytes {
		return errors.Errorf("content must be at most %d bytes", maxMemoTemplateContentBytes)
	}
	return nil
}

// ListMemoTemplates lists the reusable memo templates owned by a user.
func (s *APIV1Service) ListMemoTemplates(ctx context.Context, request *v1pb.ListMemoTemplatesRequest) (*v1pb.ListMemoTemplatesResponse, error) {
	user, err := ResolveUserByName(ctx, s.Store, request.GetParent())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid user name: %v", err)
	}
	if user == nil {
		return nil, status.Errorf(codes.NotFound, "user not found")
	}
	if err := s.requireCallerIs(ctx, user); err != nil {
		return nil, err
	}

	storeMemoTemplates, err := s.Store.GetUserMemoTemplates(ctx, user.ID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get memo templates: %v", err)
	}

	memoTemplates := make([]*v1pb.MemoTemplate, 0, len(storeMemoTemplates))
	for _, memoTemplate := range storeMemoTemplates {
		if memoTemplate != nil {
			memoTemplates = append(memoTemplates, convertMemoTemplateFromStore(user.Username, memoTemplate))
		}
	}

	return &v1pb.ListMemoTemplatesResponse{MemoTemplates: memoTemplates}, nil
}

// GetMemoTemplate returns a reusable memo template by resource name.
func (s *APIV1Service) GetMemoTemplate(ctx context.Context, request *v1pb.GetMemoTemplateRequest) (*v1pb.MemoTemplate, error) {
	user, memoTemplateID, err := s.extractUserAndMemoTemplateIDFromName(ctx, request.GetName())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid memo template name: %v", err)
	}
	if err := s.requireCallerIs(ctx, user); err != nil {
		return nil, err
	}

	memoTemplates, err := s.Store.GetUserMemoTemplates(ctx, user.ID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get memo templates: %v", err)
	}
	for _, memoTemplate := range memoTemplates {
		if memoTemplate.GetId() == memoTemplateID {
			return convertMemoTemplateFromStore(user.Username, memoTemplate), nil
		}
	}

	return nil, status.Errorf(codes.NotFound, "memo template not found")
}

// CreateMemoTemplate creates a reusable memo template for a user.
func (s *APIV1Service) CreateMemoTemplate(ctx context.Context, request *v1pb.CreateMemoTemplateRequest) (*v1pb.MemoTemplate, error) {
	user, err := ResolveUserByName(ctx, s.Store, request.GetParent())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid user name: %v", err)
	}
	if user == nil {
		return nil, status.Errorf(codes.NotFound, "user not found")
	}
	if err := s.requireCallerIs(ctx, user); err != nil {
		return nil, err
	}
	if err := s.throttleAndCharge(ratelimit.ScopeWriteUser, userKey(auth.GetUserID(ctx)), 1); err != nil {
		return nil, err
	}

	incoming := request.GetMemoTemplate()
	title, err := validateMemoTemplate(incoming.GetTitle(), incoming.GetContent())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid memo template: %v", err)
	}
	newMemoTemplate := &storepb.MemoTemplatesUserSetting_MemoTemplate{
		Id:      random.UUID(),
		Title:   title,
		Content: incoming.GetContent(),
	}
	if request.GetValidateOnly() {
		return convertMemoTemplateFromStore(user.Username, newMemoTemplate), nil
	}

	if err := s.Store.AddUserMemoTemplate(ctx, user.ID, newMemoTemplate); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create memo template: %v", err)
	}

	return convertMemoTemplateFromStore(user.Username, newMemoTemplate), nil
}

// UpdateMemoTemplate updates the selected fields of a reusable memo template.
func (s *APIV1Service) UpdateMemoTemplate(ctx context.Context, request *v1pb.UpdateMemoTemplateRequest) (*v1pb.MemoTemplate, error) {
	incoming := request.GetMemoTemplate()
	user, memoTemplateID, err := s.extractUserAndMemoTemplateIDFromName(ctx, incoming.GetName())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid memo template name: %v", err)
	}
	if err := s.requireCallerIs(ctx, user); err != nil {
		return nil, err
	}

	paths := []string{"title", "content"}
	if request.GetUpdateMask() != nil && len(request.GetUpdateMask().GetPaths()) > 0 {
		paths = request.GetUpdateMask().GetPaths()
	}

	var title, content *string
	for _, field := range paths {
		switch field {
		case "title":
			value, err := validateMemoTemplateTitle(incoming.GetTitle())
			if err != nil {
				return nil, status.Errorf(codes.InvalidArgument, "invalid memo template: %v", err)
			}
			title = &value
		case "content":
			if err := validateMemoTemplateContent(incoming.GetContent()); err != nil {
				return nil, status.Errorf(codes.InvalidArgument, "invalid memo template: %v", err)
			}
			value := incoming.GetContent()
			content = &value
		default:
			return nil, status.Errorf(codes.InvalidArgument, "unsupported update mask path: %s", field)
		}
	}

	updatedMemoTemplate, err := s.Store.UpdateUserMemoTemplate(ctx, user.ID, memoTemplateID, title, content)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to update memo template: %v", err)
	}
	if updatedMemoTemplate == nil {
		return nil, status.Errorf(codes.NotFound, "memo template not found")
	}

	return convertMemoTemplateFromStore(user.Username, updatedMemoTemplate), nil
}

// DeleteMemoTemplate deletes a reusable memo template by resource name.
func (s *APIV1Service) DeleteMemoTemplate(ctx context.Context, request *v1pb.DeleteMemoTemplateRequest) (*emptypb.Empty, error) {
	user, memoTemplateID, err := s.extractUserAndMemoTemplateIDFromName(ctx, request.GetName())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid memo template name: %v", err)
	}
	if err := s.requireCallerIs(ctx, user); err != nil {
		return nil, err
	}

	found, err := s.Store.RemoveUserMemoTemplate(ctx, user.ID, memoTemplateID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to delete memo template: %v", err)
	}
	if !found {
		return nil, status.Errorf(codes.NotFound, "memo template not found")
	}

	return &emptypb.Empty{}, nil
}
