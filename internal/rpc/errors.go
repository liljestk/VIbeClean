package rpc

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/liljestk/VIbeClean/internal/model"
)

// toStatus maps a backend error to a gRPC status, keeping its message.
func toStatus(err error) error {
	if err == nil {
		return nil
	}
	code := codes.Internal
	switch {
	case errors.Is(err, context.Canceled):
		code = codes.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		code = codes.DeadlineExceeded
	case errors.Is(err, model.ErrNotFound):
		code = codes.NotFound
	case errors.Is(err, model.ErrInvalidArgument):
		code = codes.InvalidArgument
	case errors.Is(err, model.ErrConflict):
		code = codes.Aborted
	case errors.Is(err, model.ErrUnavailable):
		code = codes.Unavailable
	}
	return status.Error(code, err.Error())
}

// fromStatus maps a gRPC status back to the error a local backend would
// return, so front ends handle remote and local failures the same way.
func fromStatus(err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	message := st.Message()
	switch st.Code() {
	case codes.Canceled:
		return context.Canceled
	case codes.DeadlineExceeded:
		return context.DeadlineExceeded
	case codes.NotFound:
		return model.NotFound("%s", message)
	case codes.InvalidArgument:
		return model.InvalidArgument("%s", message)
	case codes.Aborted:
		return model.Conflict("%s", message)
	case codes.Unavailable:
		return model.Unavailable("%s", message)
	case codes.Unauthenticated, codes.PermissionDenied:
		return model.InvalidArgument("the Space Review API refused the token: %s", message)
	}
	return errors.New(message)
}
