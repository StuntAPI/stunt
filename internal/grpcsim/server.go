// Package grpcsim provides dynamic gRPC serving from a protobuf
// FileDescriptorSet — no compiled Go stubs required.
//
// A [Service] pairs a fully-qualified service name and a descriptor set with a
// set of Go handler functions, each receiving the decoded request as a Go
// map[string]any and returning a response map (or a gRPC status error).
// [BuildServiceDesc] turns this into a [grpc.ServiceDesc] that the gRPC
// runtime can register, and [Serve] wires it onto a listener.
package grpcsim

import (
	"context"
	"encoding/json"
	"fmt"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Handler is a Go function that processes a single gRPC request. fullMethod is
// the gRPC method path (e.g. "/pkg.Svc/Method"). The request is decoded into
// req as a JSON-like Go map; the handler returns a response map or a non-nil
// error (which becomes a gRPC status — see [Error]).
type Handler func(ctx context.Context, fullMethod string, req map[string]any) (resp map[string]any, err error)

// Service describes a dynamically-served gRPC service. Methods is keyed by the
// bare method name (e.g. "CreateCharge").
type Service struct {
	// FullName is the fully-qualified protobuf service name, e.g.
	// "stunt.test.Greeter".
	FullName string

	// Descriptor is the FileDescriptorSet containing the service definition.
	Descriptor *descriptorpb.FileDescriptorSet

	// Methods maps a bare method name (e.g. "SayHello") to its Handler.
	Methods map[string]Handler
}

// codedError carries an explicit gRPC code alongside a message so that handlers
// can return structured errors that [BuildServiceDesc] maps to a gRPC status.
type codedError struct {
	code codes.Code
	msg  string
}

func (e *codedError) Error() string { return e.msg }

// GRPCStatus lets the grpc/status package extract the code directly when the
// error is surfaced to the client.
func (e *codedError) GRPCStatus() *status.Status {
	return status.New(e.code, e.msg)
}

// Error returns an error that maps to the given gRPC status code and message.
// Handlers should return Error(codes.NotFound, "not found") to produce a
// structured gRPC error.
func Error(code codes.Code, msg string) error {
	return &codedError{code: code, msg: msg}
}

// BuildServiceDesc constructs a dynamic [grpc.ServiceDesc] from svc. Each unary
// method declared in the descriptor is routed to the matching entry in
// svc.Methods (keyed by bare method name).
func BuildServiceDesc(svc *Service) (*grpc.ServiceDesc, error) {
	files, err := protodesc.NewFiles(svc.Descriptor)
	if err != nil {
		return nil, fmt.Errorf("grpcsim: parse descriptor set: %w", err)
	}

	svcDesc, err := files.FindDescriptorByName(protoreflect.FullName(svc.FullName))
	if err != nil {
		return nil, fmt.Errorf("grpcsim: find service %q: %w", svc.FullName, err)
	}
	service, ok := svcDesc.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil, fmt.Errorf("grpcsim: %q is not a service", svc.FullName)
	}

	desc := &grpc.ServiceDesc{
		ServiceName: svc.FullName,
		// HandlerType is a unique placeholder interface pointer. It exists
		// only so that two dynamically-built services with the same method
		// names do not collide inside grpc.Server. The method-handler
		// closures ignore srv, so no real implementation is needed; Serve
		// registers a nil impl, which skips grpc's type-check.
		HandlerType: (*serviceKey)(nil),
		Metadata:    svc.Descriptor,
	}

	methods := service.Methods()
	for i := 0; i < methods.Len(); i++ {
		md := methods.Get(i)

		handler, ok := svc.Methods[string(md.Name())]
		if !ok {
			// Skip methods with no Go handler registered.
			continue
		}

		fullMethod := "/" + svc.FullName + "/" + string(md.Name())

		desc.Methods = append(desc.Methods, grpc.MethodDesc{
			MethodName: string(md.Name()),
			Handler:    methodHandler(fullMethod, md, handler),
		})
	}

	if len(desc.Methods) == 0 {
		return nil, fmt.Errorf("grpcsim: no handlers matched methods on service %q", svc.FullName)
	}

	return desc, nil
}

// serviceKey is a private placeholder type used solely as a unique HandlerType
// pointer for each dynamically built ServiceDesc.
type serviceKey struct{}

// methodHandler builds the grpc.MethodHandler closure for a single method. It
// decodes the incoming protobuf bytes into a dynamic message, converts that to
// a Go map, calls the user handler, then converts the returned map back into a
// protobuf message for the wire.
func methodHandler(fullMethod string, md protoreflect.MethodDescriptor, h Handler) grpc.MethodHandler {
	return func(_ any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
		reqMsg := dynamicpb.NewMessage(md.Input())
		if err := dec(reqMsg); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "grpcsim: decode request: %v", err)
		}

		reqMap, err := protoToMap(reqMsg)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "grpcsim: convert request: %v", err)
		}

		respMap, err := h(ctx, fullMethod, reqMap)
		if err != nil {
			return nil, toStatusError(err)
		}

		respMsg := dynamicpb.NewMessage(md.Output())
		if err := mapToProto(respMap, respMsg); err != nil {
			return nil, status.Errorf(codes.Internal, "grpcsim: convert response: %v", err)
		}
		return respMsg, nil
	}
}

// toStatusError maps a handler error to a gRPC status error. If the error
// already carries a gRPC status (via status.Error or a GRPCStatus method), it
// is passed through unchanged.
func toStatusError(err error) error {
	if st, ok := status.FromError(err); ok && st.Code() != codes.OK {
		return st.Err()
	}
	return status.Error(codes.Internal, err.Error())
}

// protoToMap converts a protobuf message to a Go map by marshalling via
// protojson then unmarshalling the JSON into map[string]any. This preserves
// field names and nested structures in a JSON-like form.
func protoToMap(msg *dynamicpb.Message) (map[string]any, error) {
	b, err := protojson.Marshal(msg)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// mapToProto converts a Go map into a protobuf message by marshalling the map
// to JSON then unmarshalling via protojson into the dynamic message.
func mapToProto(m map[string]any, msg *dynamicpb.Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(b, msg)
}

// Serve creates a new grpc.Server, registers the dynamic service description
// derived from svc, and serves on lis. Serving happens in a goroutine; the
// returned server can be stopped via GracefulStop (the caller should also
// cancel ctx).
func Serve(ctx context.Context, svc *Service, lis net.Listener) (*grpc.Server, error) {
	desc, err := BuildServiceDesc(svc)
	if err != nil {
		return nil, err
	}

	server := grpc.NewServer()
	// RegisterService with a nil impl skips grpc's reflect-based HandlerType
	// type-check (which would panic on a non-interface placeholder). The
	// method-handler closures ignore srv entirely.
	server.RegisterService(desc, nil)

	go server.Serve(lis)

	go func() {
		<-ctx.Done()
		server.GracefulStop()
	}()

	return server, nil
}
