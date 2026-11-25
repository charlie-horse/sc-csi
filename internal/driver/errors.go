package driver

import (
	"context"
	"errors"
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/charlie-horse/sc-csi/internal/csp"
	"github.com/charlie-horse/sc-csi/internal/sc"
)

// toStatus turns a csp, sc or context error into a gRPC status error. A csp 409 becomes
// conflict, which callers pick per RPC (AlreadyExists or FailedPrecondition).
func toStatus(err error, conflict codes.Code) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	var ce *csp.Error
	var se *sc.Error
	switch {
	case errors.As(err, &ce):
		return status.Error(cspCode(ce.Status, conflict), ce.Msg)
	case errors.As(err, &se):
		switch {
		case se.Status == http.StatusUnauthorized:
			return status.Error(codes.Unauthenticated, err.Error())
		case se.Status >= 500:
			return status.Error(codes.Unavailable, err.Error())
		}
		return status.Error(codes.Internal, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}

func cspCode(httpStatus int, conflict codes.Code) codes.Code {
	switch httpStatus {
	case http.StatusBadRequest:
		return codes.InvalidArgument
	case http.StatusNotFound:
		return codes.NotFound
	case http.StatusConflict:
		return conflict
	case http.StatusUnauthorized:
		return codes.Unauthenticated
	case http.StatusNotImplemented:
		return codes.Unimplemented
	case http.StatusServiceUnavailable:
		return codes.Unavailable
	}
	return codes.Internal
}

// isNotFound reports whether err is a csp 404.
func isNotFound(err error) bool {
	var ce *csp.Error
	return errors.As(err, &ce) && ce.Status == http.StatusNotFound
}
