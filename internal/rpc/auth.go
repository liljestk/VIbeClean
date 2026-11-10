package rpc

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net"
	"path/filepath"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	authorization = "authorization"
	bearerPrefix  = "Bearer "
	tokenBytes    = 32
	// healthPrefix lets orchestrators check liveness without the token.
	healthPrefix = "/grpc.health.v1.Health/"
)

// DefaultTokenFile keeps the ACL-protected API token beside the index.
func DefaultTokenFile(indexPath string) string {
	return filepath.Join(filepath.Dir(indexPath), "api-token")
}

// checkLoopback refuses listen addresses other than loopback. The API has no
// transport encryption, so remote access needs a tunnel the user controls.
func checkLoopback(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("listen on a loopback address such as 127.0.0.1, not %q", host)
}

type authenticator struct {
	token []byte
}

func (a authenticator) check(ctx context.Context, method string) error {
	if strings.HasPrefix(method, healthPrefix) {
		return nil
	}
	values := metadata.ValueFromIncomingContext(ctx, authorization)
	if len(values) == 1 && strings.HasPrefix(values[0], bearerPrefix) &&
		subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(values[0], bearerPrefix)), a.token) == 1 {
		return nil
	}
	return status.Error(codes.Unauthenticated, "missing or invalid bearer token")
}

func (a authenticator) unary(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if err := a.check(ctx, info.FullMethod); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

func (a authenticator) stream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if err := a.check(ss.Context(), info.FullMethod); err != nil {
		return err
	}
	return handler(srv, ss)
}

// bearer attaches the token to every call.
type bearer string

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{authorization: bearerPrefix + string(b)}, nil
}

// RequireTransportSecurity is false because the API only listens on loopback.
func (bearer) RequireTransportSecurity() bool { return false }
