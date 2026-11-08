// Package rpc serves the backend over gRPC and provides a client that
// implements the same backend interface, so any front end can run against a
// local or a remote backend without changes.
package rpc

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	pb "github.com/liljestk/VIbeClean/gen/spacereview/v1"
	"github.com/liljestk/VIbeClean/internal/diag"
	"github.com/liljestk/VIbeClean/internal/service"
)

// shutdownGrace is how long a stopping server waits for calls in progress.
const shutdownGrace = 5 * time.Second

// Server adapts a backend to the generated gRPC service.
type Server struct {
	pb.UnimplementedSpaceReviewServiceServer
	backend service.Backend
}

// NewServer returns a gRPC service implementation over a backend.
func NewServer(backend service.Backend) *Server {
	return &Server{backend: backend}
}

// NewGRPCServer builds a gRPC server with token authentication, health
// checks, reflection and the Space Review service registered.
func NewGRPCServer(ctx context.Context, backend service.Backend, token string) *grpc.Server {
	auth := authenticator{token: []byte(token)}
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(auth.unary, logUnary(ctx)),
		grpc.ChainStreamInterceptor(auth.stream, logStream(ctx)),
	)
	pb.RegisterSpaceReviewServiceServer(srv, NewServer(backend))
	healthpb.RegisterHealthServer(srv, health.NewServer())
	reflection.Register(srv)
	return srv
}

// Serve listens on a loopback address until ctx ends. ready, when set, is
// called with the bound address once the server accepts calls.
func Serve(ctx context.Context, backend service.Backend, address, token string, ready func(net.Addr)) error {
	if err := checkLoopback(address); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	srv := NewGRPCServer(ctx, backend, token)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		done := make(chan struct{})
		go func() {
			srv.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(shutdownGrace):
			srv.Stop()
		}
	}()
	if ready != nil {
		ready(listener.Addr())
	}
	err = srv.Serve(listener)
	<-stopped
	if errors.Is(err, grpc.ErrServerStopped) {
		return nil
	}
	return err
}

// logUnary times each call. Requests are not logged because they can carry
// folder paths and search text.
func logUnary(base context.Context) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx = diag.Operation(diag.Inherit(ctx, base))
		started := time.Now()
		reply, err := handler(ctx, req)
		diag.Duration(ctx, "rpc.request", started, err, slog.String("method", info.FullMethod))
		return reply, err
	}
}

func logStream(base context.Context) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx := diag.Operation(diag.Inherit(ss.Context(), base))
		started := time.Now()
		err := handler(srv, &tracedStream{ServerStream: ss, ctx: ctx})
		diag.Duration(ctx, "rpc.request", started, err, slog.String("method", info.FullMethod))
		return err
	}
}

type tracedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *tracedStream) Context() context.Context { return s.ctx }

// GetServerInfo identifies the server.
func (s *Server) GetServerInfo(ctx context.Context, _ *pb.GetServerInfoRequest) (*pb.GetServerInfoResponse, error) {
	info, err := s.backend.Info(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	return &pb.GetServerInfoResponse{Version: info.Version, IndexPath: info.IndexPath}, nil
}

// ListDrives returns current fixed-drive readings.
func (s *Server) ListDrives(ctx context.Context, _ *pb.ListDrivesRequest) (*pb.ListDrivesResponse, error) {
	drives, err := s.backend.Drives(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	return &pb.ListDrivesResponse{Drives: drivesToPB(drives)}, nil
}

// ListScopes returns the latest capture of each scope.
func (s *Server) ListScopes(ctx context.Context, _ *pb.ListScopesRequest) (*pb.ListScopesResponse, error) {
	scopes, err := s.backend.Scopes(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	reply := &pb.ListScopesResponse{Scopes: make([]*pb.Snapshot, 0, len(scopes))}
	for _, scope := range scopes {
		reply.Scopes = append(reply.Scopes, snapshotToPB(scope))
	}
	return reply, nil
}

// GetLatest returns the latest capture of a scope.
func (s *Server) GetLatest(ctx context.Context, req *pb.GetLatestRequest) (*pb.GetLatestResponse, error) {
	latest, err := s.backend.Latest(ctx, service.LatestQuery{Scope: req.GetScope(), Series: seriesName(req.GetSeries()),
		Query: req.GetQuery(), Limit: int(req.GetLimit())})
	if err != nil {
		return nil, toStatus(err)
	}
	return &pb.GetLatestResponse{Snapshot: snapshotToPB(latest.Snapshot), RefreshRecommended: latest.RefreshRecommended}, nil
}

// CompareCaptures ranks changes between two captures.
func (s *Server) CompareCaptures(ctx context.Context, req *pb.CompareCapturesRequest) (*pb.CompareCapturesResponse, error) {
	diff, err := s.backend.Compare(ctx, service.CompareQuery{Scope: req.GetScope(), Days: int(req.GetDays()),
		Series: seriesName(req.GetSeries()), MinimumBytes: req.GetMinimumBytes(), Direction: directionFromPB(req.GetDirection()),
		Query: req.GetQuery(), Limit: int(req.GetLimit())})
	if err != nil {
		return nil, toStatus(err)
	}
	return comparisonToPB(diff), nil
}

// Capture measures a scope, streaming progress and then the saved snapshot.
func (s *Server) Capture(req *pb.CaptureRequest, stream pb.SpaceReviewService_CaptureServer) error {
	saved, err := s.backend.Capture(stream.Context(), service.CaptureRequest{Scope: req.GetScope(), Kind: req.GetKind()},
		func(p service.Progress) {
			// A failed send means the client went away; the context cancels the capture.
			_ = stream.Send(&pb.CaptureResponse{Event: &pb.CaptureResponse_Progress{
				Progress: &pb.CaptureProgress{FilesChecked: p.FilesChecked}}})
		})
	if err != nil {
		return toStatus(err)
	}
	return stream.Send(&pb.CaptureResponse{Event: &pb.CaptureResponse_Snapshot{Snapshot: snapshotToPB(saved)}})
}
