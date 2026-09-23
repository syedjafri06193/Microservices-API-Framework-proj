package interceptor

import (
	"context"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
)

// Validator checks a request message.
//
// An interface rather than a hard dependency on protovalidate. The design
// document names protovalidate, and it is the right choice for most
// services — but it pulls in cel-go, which is a large tree to impose on
// every user of the framework, and the framework's own rule is that its
// dependency list becomes everyone's dependency list.
//
// So the interceptor takes an interface that protovalidate.Validator
// already satisfies:
//
//	v, _ := protovalidate.New()
//	service.WithValidator(v)
//
// Teams that want it pay for it; teams that validate another way, or in the
// handler, pay nothing. See docs/dependencies.md.
type Validator interface {
	Validate(msg proto.Message) error
}

// ValidatorFunc adapts a function to the Validator interface.
type ValidatorFunc func(proto.Message) error

// Validate calls f.
func (f ValidatorFunc) Validate(m proto.Message) error { return f(m) }

// Validation rejects malformed requests before the handler runs.
//
// A validation failure is CodeInvalidArgument, which the breaker classifies
// as a client fault — so a caller sending bad messages gets errors and does
// not take the service down for everybody else.
func Validation(v Validator) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if v == nil {
				return next(ctx, req)
			}
			msg, ok := req.Any().(proto.Message)
			if !ok {
				// Not a protobuf message. Connect supports other codecs,
				// and a non-proto request is not something this validator
				// can speak to; passing it through is more honest than
				// rejecting it.
				return next(ctx, req)
			}
			if err := v.Validate(msg); err != nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, err)
			}
			return next(ctx, req)
		}
	}
}
