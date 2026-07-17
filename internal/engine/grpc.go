package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"

	"github.com/stunt-adapters/stunt/internal/adapter"
	"github.com/stunt-adapters/stunt/internal/grpcsim"
	"github.com/stunt-adapters/stunt/internal/starlark"
	sk "go.starlark.net/starlark"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// httpStatusToGRPC maps an HTTP status code (as returned by a Starlark handler)
// to the closest gRPC status code. This lets gRPC handlers reuse the same
// respond(status, body) pattern as HTTP handlers.
func httpStatusToGRPC(status int) codes.Code {
	switch {
	case status >= 200 && status < 300:
		return codes.OK
	case status == 401:
		return codes.Unauthenticated
	case status == 403:
		return codes.PermissionDenied
	case status == 404:
		return codes.NotFound
	case status == 400:
		return codes.InvalidArgument
	case status >= 400 && status < 500:
		return codes.InvalidArgument
	case status >= 500:
		return codes.Internal
	default:
		return codes.OK
	}
}

// startGRPC builds a grpcsim.Service from the adapter's GrpcSpec, starts a
// gRPC server on a free loopback port, and returns the dial target plus the
// running server (for lifecycle management). Each gRPC method is routed to a
// Starlark handler via buildGRPCHandler.
func (e *Engine) startGRPC(ctx context.Context, st *serviceState) (string, *grpc.Server, error) {
	spec := st.adapter.Grpc

	descBytes, err := st.adapter.DescriptorBytes()
	if err != nil {
		return "", nil, fmt.Errorf("engine: read grpc descriptor: %w", err)
	}

	fds := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(descBytes, fds); err != nil {
		return "", nil, fmt.Errorf("engine: parse grpc descriptor: %w", err)
	}

	// Determine which methods are streaming by inspecting the descriptor.
	streamingNames, err := detectStreamingMethods(fds, spec.Service)
	if err != nil {
		return "", nil, fmt.Errorf("engine: inspect grpc descriptor: %w", err)
	}

	// Build a handler for each declared method — streaming methods get a
	// StreamHandler, unary methods get a Handler.
	methods := make(map[string]grpcsim.Handler, len(spec.Methods))
	streams := make(map[string]grpcsim.StreamHandler)
	for _, m := range spec.Methods {
		if streamingNames[m.Name] {
			streams[m.Name] = e.buildGRPCStreamHandler(st, m.Handler)
		} else {
			methods[m.Name] = e.buildGRPCHandler(st, m.Handler)
		}
	}

	svc := &grpcsim.Service{
		FullName:   spec.Service,
		Descriptor: fds,
		Methods:    methods,
		Streams:    streams,
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("engine: grpc listen: %w", err)
	}

	srv, result, err := grpcsim.Serve(ctx, svc, lis)
	if err != nil {
		lis.Close()
		return "", nil, fmt.Errorf("engine: grpc serve: %w", err)
	}

	// Surface serve errors (e.g. listener failure) so they are not silently
	// swallowed. We check asynchronously to avoid blocking the start path.
	go func() {
		if serveErr := result.Wait(); serveErr != nil {
			// The server has stopped; the engine's Close() handles cleanup.
			fmt.Fprintf(os.Stderr, "engine: grpc serve error: %v\n", serveErr)
		}
	}()

	return lis.Addr().String(), srv, nil
}

// buildGRPCHandler creates a grpcsim.Handler that dispatches to a Starlark
// handler function. The gRPC request map is passed as starlark.Request.Body;
// the Starlark Response.Body is returned as the gRPC response. Response.Status
// is mapped to a gRPC code via httpStatusToGRPC. Handler errors and panics
// are caught and mapped to codes.Internal so the server never crashes.
func (e *Engine) buildGRPCHandler(st *serviceState, handlerSpec string) grpcsim.Handler {
	scriptPath, fnName := adapter.SplitHandler(handlerSpec)

	return func(ctx context.Context, fullMethod string, req map[string]any) (resp map[string]any, err error) {
		// Recover from panics inside the Starlark VM so a buggy handler
		// never crashes the gRPC server.
		defer func() {
			if r := recover(); r != nil {
				err = grpcsim.Error(codes.Internal, fmt.Sprintf("handler panic: %v", r))
			}
		}()

		vm, err := st.getOrLoadVM(scriptPath)
		if err != nil {
			return nil, grpcsim.Error(codes.Internal, fmt.Sprintf("load handler script: %v", err))
		}

		starReq := starlark.Request{
			Method: "GRPC",
			Path:   fullMethod,
			Body:   req,
		}

		result, err := vm.Call(fnName, starReq)
		if err != nil {
			return nil, grpcsim.Error(codes.Internal, fmt.Sprintf("handler error: %v", err))
		}

		if code := httpStatusToGRPC(result.Status); code != codes.OK {
			msg := errMsgFromBody(result.Body)
			return nil, grpcsim.Error(code, msg)
		}

		return result.Body, nil
	}
}

// errMsgFromBody extracts a human-readable error message from a response body
// map. If the body has an "error" key, its string value is used; otherwise a
// generic message is returned.
func errMsgFromBody(body map[string]any) string {
	if body != nil {
		if e, ok := body["error"]; ok {
			if s, ok := e.(string); ok && s != "" {
				return s
			}
		}
	}
	return "request failed"
}

// detectStreamingMethods inspects the protobuf descriptor and returns the set
// of bare method names that are streaming (client-streaming or
// server-streaming). This lets the engine route streaming methods to
// buildGRPCStreamHandler and unary methods to buildGRPCHandler.
func detectStreamingMethods(fds *descriptorpb.FileDescriptorSet, serviceFullName string) (map[string]bool, error) {
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		return nil, fmt.Errorf("parse descriptor set: %w", err)
	}
	desc, err := files.FindDescriptorByName(protoreflect.FullName(serviceFullName))
	if err != nil {
		return nil, fmt.Errorf("find service %q: %w", serviceFullName, err)
	}
	service, ok := desc.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil, fmt.Errorf("%q is not a service", serviceFullName)
	}

	streaming := make(map[string]bool)
	methods := service.Methods()
	for i := 0; i < methods.Len(); i++ {
		md := methods.Get(i)
		if md.IsStreamingClient() || md.IsStreamingServer() {
			streaming[string(md.Name())] = true
		}
	}
	return streaming, nil
}

// buildGRPCStreamHandler creates a grpcsim.StreamHandler that dispatches to a
// Starlark streaming handler function on_<method>(stream). The handler uses
// stream.recv() and stream.send() to interact with the client. The handler's
// return value controls the trailing gRPC status:
//
//   - None (implicit) or respond(200, ...) → OK. If a non-nil body dict is
//     returned with a 2xx status, it is sent as a final message (useful for
//     client-streaming where the single response comes from the return value).
//   - respond(4xx/5xx, ...) → mapped to a gRPC error code via
//     httpStatusToGRPC.
//   - A raised exception or panic → codes.Internal.
func (e *Engine) buildGRPCStreamHandler(st *serviceState, handlerSpec string) grpcsim.StreamHandler {
	scriptPath, fnName := adapter.SplitHandler(handlerSpec)

	return func(ctx context.Context, fullMethod string, stream grpcsim.Stream) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = grpcsim.Error(codes.Internal, fmt.Sprintf("handler panic: %v", r))
			}
		}()

		vm, err := st.getOrLoadVM(scriptPath)
		if err != nil {
			return grpcsim.Error(codes.Internal, fmt.Sprintf("load handler script: %v", err))
		}

		sv := &streamValue{stream: stream}
		result, err := vm.CallWith(fnName, sv)
		if err != nil {
			return grpcsim.Error(codes.Internal, fmt.Sprintf("handler error: %v", err))
		}

		// Non-OK status → gRPC error (no final message sent).
		if code := httpStatusToGRPC(result.Status); code != codes.OK {
			return grpcsim.Error(code, errMsgFromBody(result.Body))
		}

		// OK status with a non-nil body → send as a final message. This
		// handles client-streaming where the single response comes from the
		// return value (return respond(200, {"total": total})).
		if result.Body != nil {
			if sendErr := stream.Send(result.Body); sendErr != nil {
				return sendErr
			}
		}

		return nil
	}
}

// streamValue is a Starlark value that exposes recv() and send() methods to a
// streaming gRPC handler script. It wraps a grpcsim.Stream, converting between
// Go maps and Starlark dicts.
//
// In Starlark:
//
//	msg = stream.recv()   # returns a dict, or None on client half-close
//	stream.send({"k": v})  # sends a message; returns None
type streamValue struct {
	stream grpcsim.Stream
}

// String implements sk.Value.
func (s *streamValue) String() string  { return "stream" }

// Type implements sk.Value.
func (s *streamValue) Type() string  { return "stream" }

// Freeze implements sk.Value. The stream is inherently per-call and not
// frozen — handlers operate on it within a single invocation.
func (s *streamValue) Freeze() {}

// Truth implements sk.Value.
func (s *streamValue) Truth() sk.Bool { return true }

// Hash implements sk.Value.
func (s *streamValue) Hash() (uint32, error) { return 0, fmt.Errorf("stream is unhashable") }

// Attr implements sk.HasAttrs, returning the recv and send builtins.
func (s *streamValue) Attr(name string) (sk.Value, error) {
	switch name {
	case "recv":
		return sk.NewBuiltin("recv", s.recv), nil
	case "send":
		return sk.NewBuiltin("send", s.send), nil
	default:
		return nil, nil // no such attribute
	}
}

// AttrNames implements sk.HasAttrs.
func (s *streamValue) AttrNames() []string {
	return []string{"recv", "send"}
}

// recv reads the next inbound message. It returns a Starlark dict, or None
// when the client has half-closed (io.EOF).
func (s *streamValue) recv(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	if err := sk.UnpackArgs("recv", args, kwargs); err != nil {
		return nil, err
	}
	msg, err := s.stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return sk.None, nil // client half-close → None
		}
		return nil, err
	}
	return starlark.GoToStarlark(msg), nil
}

// send writes an outbound message. The argument must be a Starlark dict,
// which is converted to a Go map and passed to stream.Send.
func (s *streamValue) send(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	var msgVal sk.Value
	if err := sk.UnpackArgs("send", args, kwargs, "msg", &msgVal); err != nil {
		return nil, err
	}
	dict, ok := msgVal.(*sk.Dict)
	if !ok {
		return nil, fmt.Errorf("send: msg must be a dict, got %s", msgVal.Type())
	}
	if err := s.stream.Send(starlark.StarlarkToGo(dict)); err != nil {
		return nil, err
	}
	return sk.None, nil
}
