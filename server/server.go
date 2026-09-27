package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"
	"uuid"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
	"github.com/pkg/errors"

	"github.com/usememos/memos/core/digest"
	"github.com/usememos/memos/internal/clientip"
	"github.com/usememos/memos/internal/profile"
	"github.com/usememos/memos/markdown"
	storepb "github.com/usememos/memos/proto/gen/store"
	apiv1 "github.com/usememos/memos/server/api/v1"
	"github.com/usememos/memos/server/fileserver"
	"github.com/usememos/memos/server/frontend"
	"github.com/usememos/memos/server/mcp"
	"github.com/usememos/memos/store"
)

const (
	shutdownTimeout = 10 * time.Second

	// readHeaderTimeout bounds how long a client may take to send request
	// headers, so idle or slow connections cannot pin a worker forever. Bodies
	// and responses are not bounded here: uploads and SSE streams are
	// legitimately long, and the request context still ends on disconnect.
	readHeaderTimeout = 15 * time.Second
	// idleTimeout closes keep-alive connections that send nothing.
	idleTimeout = 2 * time.Minute
)

type Server struct {
	Secret  string
	Profile *profile.Profile
	Store   *store.Store

	echoServer         *echo.Echo
	httpServer         *http.Server
	apiV1Service       *apiv1.APIV1Service
	weeklyDigestRunner *WeeklyDigestRunner
}

func NewServer(ctx context.Context, profile *profile.Profile, storeInstance *store.Store) (*Server, error) {
	s := &Server{
		Store:   storeInstance,
		Profile: profile,
	}

	echoServer := echo.New()
	echoServer.Use(middleware.Recover())
	echoServer.Use(newCORSMiddleware(profile))
	// Resolve the client address once per request, before anything that keys
	// on it: rate limits, session records, and the file server.
	clientIPResolver, err := clientip.ParseTrustedProxies(profile.TrustedProxies)
	if err != nil {
		return nil, errors.Wrap(err, "invalid trusted proxies")
	}
	echoServer.Use(clientip.Middleware(clientIPResolver))
	s.echoServer = echoServer

	instanceBasicSetting, err := s.getOrUpsertInstanceBasicSetting(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get instance basic setting")
	}

	secret := "usememos"
	if !profile.Demo {
		secret = instanceBasicSetting.SecretKey
	}
	s.Secret = secret

	// Register healthz endpoint.
	echoServer.GET("/healthz", func(c *echo.Context) error {
		return c.String(http.StatusOK, "Service ready.")
	})

	// Serve frontend static files.
	frontend.NewFrontendService(profile, storeInstance).Serve(ctx, echoServer)

	apiV1Service := apiv1.NewAPIV1Service(s.Secret, profile, storeInstance)
	s.apiV1Service = apiV1Service

	// Register HTTP file server routes BEFORE gRPC-Gateway to ensure proper range request handling for Safari.
	// This uses native HTTP serving (http.ServeContent) instead of gRPC for video/audio files.
	fileServerService := fileserver.NewFileServerService(s.Profile, s.Store, s.Secret)
	fileServerService.RegisterRoutes(echoServer)

	// Register gRPC gateway as api v1 (includes SSE endpoint on CORS-enabled group).
	if err := apiV1Service.RegisterGateway(ctx, echoServer); err != nil {
		return nil, errors.Wrap(err, "failed to register gRPC gateway")
	}

	mcpService, err := mcp.NewMCPService(profile, echoServer)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create MCP service")
	}
	mcpService.RegisterRoutes(echoServer)
	if profile.WeeklyDigest {
		digestBuilder := digest.NewBuilder(storeInstance, markdown.NewService(), profile.InstanceURL)
		s.weeklyDigestRunner = NewWeeklyDigestRunner(storeInstance, WeeklyDigestRunnerOptions{
			OptIn: WeeklyDigestOptInFromStore(storeInstance),
			Deliver: func(deliveryCtx context.Context, user *store.User, period WeeklyDigestPeriod) (bool, error) {
				window, err := digest.NewWindow(period.Start, period.End)
				if err != nil {
					return false, errors.Wrap(err, "invalid weekly digest period")
				}
				setting, err := storeInstance.GetInstanceNotificationSetting(deliveryCtx)
				if err != nil {
					return false, errors.Wrap(err, "failed to get weekly digest notification setting")
				}
				return digestBuilder.Send(deliveryCtx, user, window, setting.GetEmail())
			},
		})
	}

	return s, nil
}

func (s *Server) Start() error {
	var address, network string
	if len(s.Profile.UNIXSock) == 0 {
		address = fmt.Sprintf("%s:%d", s.Profile.Addr, s.Profile.Port)
		network = "tcp"
	} else {
		address = s.Profile.UNIXSock
		network = "unix"
	}
	listener, err := net.Listen(network, address)
	if err != nil {
		return errors.Wrap(err, "failed to listen")
	}

	if network == "unix" {
		if err := os.Chmod(address, 0660); err != nil {
			_ = listener.Close()
			return errors.Wrap(err, "failed to chmod socket")
		}
	}

	// Start Echo server directly (no cmux needed - all traffic is HTTP).
	s.httpServer = &http.Server{
		Handler:           s.echoServer,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}
	go func() {
		if err := s.httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			slog.Error("failed to start echo server", "error", err)
		}
	}()
	if s.weeklyDigestRunner != nil {
		s.weeklyDigestRunner.Start()
	}

	return nil
}

func (s *Server) Shutdown(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, shutdownTimeout)
	defer cancel()

	slog.Info("server shutting down")

	s.closeLongLivedConnections()
	if s.weeklyDigestRunner != nil {
		if err := s.weeklyDigestRunner.Stop(ctx); err != nil {
			slog.Error("failed to stop weekly digest runner", slog.String("error", err.Error()))
			// The store must not close while a delivery callback can still be
			// using it. The runner has already cancelled its run context; wait
			// without the HTTP shutdown deadline before closing the store.
			if waitErr := s.weeklyDigestRunner.Stop(context.Background()); waitErr != nil {
				slog.Error("failed to wait for weekly digest runner", slog.String("error", waitErr.Error()))
			}
		}
	}
	s.shutdownHTTPServer(ctx)
	s.apiV1Service.CloseUploads()

	// Close database connection.
	if err := s.Store.Close(); err != nil {
		slog.Error("failed to close database", slog.String("error", err.Error()))
	}

	slog.Info("memos stopped properly")
}

// ConfigureWeeklyDigest wires the core digest renderer and the generated
// user-setting opt-in check into the server-owned runner. It must be called
// after NewServer and before Start when --weekly-digest is enabled.
func (s *Server) ConfigureWeeklyDigest(optIn WeeklyDigestOptIn, deliver WeeklyDigestDelivery) error {
	if s.weeklyDigestRunner == nil {
		return errors.New("weekly digest is disabled")
	}
	s.weeklyDigestRunner.SetDelivery(optIn, deliver)
	return nil
}

func (s *Server) closeLongLivedConnections() {
	// Long-lived SSE requests do not finish on their own during http.Server.Shutdown.
	s.apiV1Service.SSEHub.Close()
}

func (s *Server) shutdownHTTPServer(ctx context.Context) {
	if s.httpServer == nil {
		return
	}
	if err := s.httpServer.Shutdown(ctx); err != nil {
		slog.Error("failed to shutdown server", slog.String("error", err.Error()))
		if closeErr := s.httpServer.Close(); closeErr != nil && closeErr != http.ErrServerClosed {
			slog.Error("failed to close server", slog.String("error", closeErr.Error()))
		}
	}
}

func (s *Server) getOrUpsertInstanceBasicSetting(ctx context.Context) (*storepb.InstanceBasicSetting, error) {
	instanceBasicSetting, err := s.Store.GetInstanceBasicSetting(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get instance basic setting")
	}
	modified := false
	if instanceBasicSetting.SecretKey == "" {
		instanceBasicSetting.SecretKey = uuid.NewV4().String()
		modified = true
	}
	if modified {
		instanceSetting, err := s.Store.UpsertInstanceSetting(ctx, &storepb.InstanceSetting{
			Key:   storepb.InstanceSettingKey_BASIC,
			Value: &storepb.InstanceSetting_BasicSetting{BasicSetting: instanceBasicSetting},
		})
		if err != nil {
			return nil, errors.Wrap(err, "failed to upsert instance setting")
		}
		instanceBasicSetting = instanceSetting.GetBasicSetting()
	}
	return instanceBasicSetting, nil
}
