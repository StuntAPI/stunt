package engine

import (
	"context"
	"fmt"
	"net"
	"os"

	"github.com/stunt-adapters/stunt/internal/adapter"
	"github.com/stunt-adapters/stunt/internal/grpcsim"
	"github.com/stunt-adapters/stunt/internal/starlark"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
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

	// Build a handler for each declared method.
	methods := make(map[string]grpcsim.Handler, len(spec.Methods))
	for _, m := range spec.Methods {
		methods[m.Name] = e.buildGRPCHandler(st, m.Handler)
	}

	svc := &grpcsim.Service{
		FullName:   spec.Service,
		Descriptor: fds,
		Methods:    methods,
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
