package fwtest

import (
	"context"
	"errors"
	"sync"
	"time"

	"connectrpc.com/connect"

	echov1 "github.com/syedjafri06193/microservices-api-framework/gen/echo/v1"
	"github.com/syedjafri06193/microservices-api-framework/gen/echo/v1/echov1connect"
	"github.com/syedjafri06193/microservices-api-framework/resilience/retry"
)

// EchoServer implements the example service used by the framework's own
// integration tests, and by anyone wanting a handler to point the framework
// at while they try it out.
type EchoServer struct {
	echov1connect.UnimplementedEchoServiceHandler

	mu       sync.Mutex
	values   map[string]int32
	charges  int
	failures map[string]int
}

// NewEchoServer returns an EchoServer.
func NewEchoServer() *EchoServer {
	return &EchoServer{values: map[string]int32{}, failures: map[string]int{}}
}

// Charges returns how many times Charge was called — the number that proves
// a non-idempotent method was not retried.
func (s *EchoServer) Charges() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.charges
}

// Echo returns the message and the retry-attempt header.
func (s *EchoServer) Echo(ctx context.Context, req *connect.Request[echov1.EchoRequest]) (*connect.Response[echov1.EchoResponse], error) {
	return connect.NewResponse(&echov1.EchoResponse{
		Message: req.Msg.GetMessage(),
		Attempt: req.Header().Get(retry.HeaderPreviousAttempts),
	}), nil
}

// SetValue stores a value and returns a monotonic revision.
func (s *EchoServer) SetValue(ctx context.Context, req *connect.Request[echov1.SetValueRequest]) (*connect.Response[echov1.SetValueResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[req.Msg.GetKey()]++
	return connect.NewResponse(&echov1.SetValueResponse{Revision: s.values[req.Msg.GetKey()]}), nil
}

// Charge counts invocations. It is not annotated idempotent, so the
// framework must never call it more than the caller did.
func (s *EchoServer) Charge(ctx context.Context, req *connect.Request[echov1.ChargeRequest]) (*connect.Response[echov1.ChargeResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.charges++
	return connect.NewResponse(&echov1.ChargeResponse{ChargeId: "ch_1"}), nil
}

// Fail returns the requested code.
func (s *EchoServer) Fail(ctx context.Context, req *connect.Request[echov1.FailRequest]) (*connect.Response[echov1.FailResponse], error) {
	s.mu.Lock()
	s.failures[req.Msg.GetCode()]++
	s.mu.Unlock()
	return nil, connect.NewError(CodeFromString(req.Msg.GetCode()), errors.New("requested failure"))
}

// Panic panics, so the recovery interceptor can be exercised end to end.
func (s *EchoServer) Panic(ctx context.Context, req *connect.Request[echov1.PanicRequest]) (*connect.Response[echov1.PanicResponse], error) {
	panic("echo handler panicked on request")
}

// Slow sleeps, for deadline and load-shedding tests.
func (s *EchoServer) Slow(ctx context.Context, req *connect.Request[echov1.SlowRequest]) (*connect.Response[echov1.SlowResponse], error) {
	delay := time.Duration(req.Msg.GetDelayMs()) * time.Millisecond
	select {
	case <-time.After(delay):
		return connect.NewResponse(&echov1.SlowResponse{}), nil
	case <-ctx.Done():
		return nil, connect.NewError(connect.CodeDeadlineExceeded, ctx.Err())
	}
}

// FailCount returns how many times Fail was asked for a given code.
func (s *EchoServer) FailCount(code string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failures[code]
}

// CodeFromString maps the names used in the Echo proto to Connect codes.
func CodeFromString(s string) connect.Code {
	switch s {
	case "unavailable":
		return connect.CodeUnavailable
	case "invalid_argument":
		return connect.CodeInvalidArgument
	case "internal":
		return connect.CodeInternal
	case "not_found":
		return connect.CodeNotFound
	case "resource_exhausted":
		return connect.CodeResourceExhausted
	case "deadline_exceeded":
		return connect.CodeDeadlineExceeded
	case "canceled":
		return connect.CodeCanceled
	case "permission_denied":
		return connect.CodePermissionDenied
	default:
		return connect.CodeUnknown
	}
}
