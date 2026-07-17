// Package proto imports Protocol Buffer .proto definitions into stunt
// adapters. It compiles the .proto source in-process using protocompile
// (no external protoc required), emits a compiled FileDescriptorSet, a grpc:
// section in adapter.yaml, and synthetic Starlark stub handlers — one per RPC
// — whose response bodies are derived from the method's output message type.
//
// All generated content is SYNTHETIC — proto files carry no data; placeholders
// are used so handlers return plausible (but fake) values at runtime.
package proto

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bufbuild/protocompile"
	"github.com/bufbuild/protocompile/linker"
	"github.com/stunt-adapters/stunt/internal/adapter"
	"github.com/stunt-adapters/stunt/internal/contrib"
	"github.com/stunt-adapters/stunt/internal/rules"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"gopkg.in/yaml.v3"
)

// We use protocompile (github.com/bufbuild/protocompile) to compile .proto
// source in-process — no external protoc binary required. protocompile parses
// .proto source, resolves types, and produces fully-linked descriptors. This
// was chosen over shelling out to `protoc` because:
//   1. No external binary dependency — works in any environment.
//   2. protocompile v0.14.1 is compatible with Go 1.23.
//   3. The compilation is self-contained and deterministic.

// ImportProto compiles a .proto definition and generates a gRPC adapter
// skeleton: a FileDescriptorSet (.desc), a grpc: section in adapter.yaml, and
// synthetic Starlark stub handlers.
func ImportProto(protoBytes []byte, dir string) error {
	// 1. Compile the proto source to a FileDescriptorSet.
	fds, fileProto, err := compileProto(protoBytes)
	if err != nil {
		return err
	}

	// 2. Pick the first service.
	svc := pickService(fileProto)
	if svc == nil {
		return fmt.Errorf("proto: no service found in %q", fileProto.GetName())
	}

	pkg := fileProto.GetPackage()
	fullName := fullyQualifiedServiceName(pkg, svc.GetName())
	fileBase := strings.ToLower(svc.GetName())

	// Build a symbol table for resolving message/enum types during synthesis.
	symbols := buildSymbolTable(fileProto, pkg)

	// 3. Write the FileDescriptorSet (binary protobuf).
	descRel := "schemas/" + fileBase + ".desc"
	descData, err := proto.Marshal(fds)
	if err != nil {
		return fmt.Errorf("proto: marshal descriptor set: %w", err)
	}
	descFull := filepath.Join(dir, descRel)
	if err := os.MkdirAll(filepath.Dir(descFull), 0o755); err != nil {
		return fmt.Errorf("proto: mkdir schemas: %w", err)
	}
	if err := os.WriteFile(descFull, descData, 0o644); err != nil {
		return fmt.Errorf("proto: write descriptor: %w", err)
	}

	// 4. Generate the .star handler script.
	starRel := "scripts/" + fileBase + ".star"
	starSrc := generateStar(svc, symbols)
	if err := contrib.WriteAdapterFile(dir, starRel, starSrc); err != nil {
		return err
	}

	// 5. Merge the grpc: section into adapter.yaml.
	if err := mergeGrpcSection(dir, fullName, descRel, starRel, svc); err != nil {
		return err
	}

	return nil
}

// ---------------------------------------------------------------------------
// compilation
// ---------------------------------------------------------------------------

// compileProto parses and links a single .proto file using protocompile.
// It returns the FileDescriptorSet (wire bytes marshaled later by the caller)
// and the FileDescriptorProto for the primary file.
func compileProto(protoBytes []byte) (*descriptorpb.FileDescriptorSet, *descriptorpb.FileDescriptorProto, error) {
	const filename = "imported.proto"

	resolver := protocompile.CompositeResolver{
		protocompile.WithStandardImports(&protocompile.SourceResolver{}),
		protocompile.ResolverFunc(func(path string) (protocompile.SearchResult, error) {
			if path == filename {
				return protocompile.SearchResult{Source: bytes.NewReader(protoBytes)}, nil
			}
			return protocompile.SearchResult{}, protoregistry.NotFound
		}),
	}

	compiler := &protocompile.Compiler{Resolver: resolver}

	files, err := compiler.Compile(context.Background(), filename)
	if err != nil {
		return nil, nil, fmt.Errorf("proto: compile: %w", err)
	}
	if len(files) == 0 {
		return nil, nil, fmt.Errorf("proto: compile produced no files")
	}

	// Collect all files (including transitive deps) into a FileDescriptorSet.
	fileProtos := collectFileProtos(files)
	fds := &descriptorpb.FileDescriptorSet{File: fileProtos}

	// The primary file is the first one (the one we compiled).
	primaryProto := protodesc.ToFileDescriptorProto(files[0])

	return fds, primaryProto, nil
}

// collectFileProtos converts linker.Files (including transitive dependencies)
// into a slice of FileDescriptorProto suitable for a FileDescriptorSet.
func collectFileProtos(files linker.Files) []*descriptorpb.FileDescriptorProto {
	seen := map[string]bool{}
	var out []*descriptorpb.FileDescriptorProto

	var walk func(f protoreflect.FileDescriptor)
	walk = func(f protoreflect.FileDescriptor) {
		path := f.Path()
		if seen[path] {
			return
		}
		seen[path] = true
		out = append(out, protodesc.ToFileDescriptorProto(f))
		for i := 0; i < f.Imports().Len(); i++ {
			walk(f.Imports().Get(i).FileDescriptor)
		}
	}

	for _, f := range files {
		walk(f)
	}
	return out
}

// ---------------------------------------------------------------------------
// service selection
// ---------------------------------------------------------------------------

// pickService returns the first service in the file (or nil if there are
// none).
func pickService(f *descriptorpb.FileDescriptorProto) *descriptorpb.ServiceDescriptorProto {
	if len(f.Service) == 0 {
		return nil
	}
	return f.Service[0]
}

// fullyQualifiedServiceName returns the fully-qualified service name
// (<package>.<Service>, or just <Service> if there's no package).
func fullyQualifiedServiceName(pkg, svcName string) string {
	if pkg == "" {
		return svcName
	}
	return pkg + "." + svcName
}

// ---------------------------------------------------------------------------
// symbol table
// ---------------------------------------------------------------------------

// symbolTable maps fully-qualified type names (e.g. ".pkg.Msg" or "pkg.Msg")
// to their descriptor proto. Used for resolving field types during synthesis.
type symbolTable struct {
	messages map[string]*descriptorpb.DescriptorProto
	enums    map[string]*descriptorpb.EnumDescriptorProto
}

// buildSymbolTable walks the file's top-level and nested message/enum types
// and indexes them by fully-qualified name.
func buildSymbolTable(f *descriptorpb.FileDescriptorProto, pkg string) *symbolTable {
	st := &symbolTable{
		messages: map[string]*descriptorpb.DescriptorProto{},
		enums:    map[string]*descriptorpb.EnumDescriptorProto{},
	}
	prefix := pkg
	for _, msg := range f.MessageType {
		st.addMessage(msg, prefix)
	}
	for _, enum := range f.EnumType {
		name := qualify(prefix, enum.GetName())
		st.enums[name] = enum
	}
	return st
}

// addMessage indexes a message and recurses into its nested types.
func (st *symbolTable) addMessage(msg *descriptorpb.DescriptorProto, prefix string) {
	full := qualify(prefix, msg.GetName())
	st.messages[full] = msg
	for _, nested := range msg.NestedType {
		st.addMessage(nested, full)
	}
	for _, enum := range msg.EnumType {
		name := qualify(full, enum.GetName())
		st.enums[name] = enum
	}
}

// qualify joins a prefix and a name with a dot. If prefix is empty, returns
// just the name.
func qualify(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

// normalizeTypeName strips the leading dot from fully-qualified type names
// in the descriptor (e.g. ".pkg.Msg" -> "pkg.Msg").
func normalizeTypeName(tn string) string {
	return strings.TrimPrefix(tn, ".")
}

// ---------------------------------------------------------------------------
// Starlark handler generation
// ---------------------------------------------------------------------------

// generateStar produces the .star file content with one handler per method.
func generateStar(svc *descriptorpb.ServiceDescriptorProto, st *symbolTable) string {
	var b strings.Builder
	b.WriteString("# Synthetic gRPC handlers generated from .proto definitions.\n")
	b.WriteString("# All values are placeholders — customize as needed.\n\n")
	for _, method := range svc.Method {
		snake := toSnakeCase(method.GetName())
		b.WriteString(fmt.Sprintf("def on_%s(req):\n", snake))
		body := synthesizeMessageBody(method.GetOutputType(), st, map[string]bool{})
		b.WriteString(fmt.Sprintf("    return respond(200, %s)\n\n", body))
	}
	return b.String()
}

// synthesizeMessageBody produces a Starlark dict literal for the response
// message of an RPC. The typeName is the fully-qualified message name from
// the method descriptor (e.g. ".pkg.ReplyMessage").
func synthesizeMessageBody(typeName string, st *symbolTable, seen map[string]bool) string {
	tn := normalizeTypeName(typeName)
	if seen[tn] {
		return "{}"
	}
	msg, ok := st.messages[tn]
	if !ok {
		return "{}"
	}
	seen[tn] = true

	var pairs []string
	for _, field := range msg.Field {
		val := synthesizeFieldValue(field, st, seen)
		pairs = append(pairs, fmt.Sprintf("%q: %s", field.GetName(), val))
	}
	return "{" + strings.Join(pairs, ", ") + "}"
}

// synthesizeFieldValue produces a Starlark expression for a single field.
func synthesizeFieldValue(field *descriptorpb.FieldDescriptorProto, st *symbolTable, seen map[string]bool) string {
	// Handle repeated/map fields.
	if field.GetLabel() == descriptorpb.FieldDescriptorProto_LABEL_REPEATED {
		// Check if this is a map field (repeated message with map_entry=true).
		if field.GetType() == descriptorpb.FieldDescriptorProto_TYPE_MESSAGE {
			tn := normalizeTypeName(field.GetTypeName())
			if msg, ok := st.messages[tn]; ok && isMapEntry(msg) {
				return synthesizeMapValue(msg, st, seen)
			}
		}
		// Regular repeated: one-element list.
		return "[" + synthesizeScalar(field, st, seen) + "]"
	}

	return synthesizeScalar(field, st, seen)
}

// synthesizeScalar produces a synthetic value for a non-repeated field.
func synthesizeScalar(field *descriptorpb.FieldDescriptorProto, st *symbolTable, seen map[string]bool) string {
	switch field.GetType() {
	case descriptorpb.FieldDescriptorProto_TYPE_STRING:
		return `"{{ faker.Word }}"`
	case descriptorpb.FieldDescriptorProto_TYPE_INT32, descriptorpb.FieldDescriptorProto_TYPE_INT64,
		descriptorpb.FieldDescriptorProto_TYPE_UINT32, descriptorpb.FieldDescriptorProto_TYPE_UINT64,
		descriptorpb.FieldDescriptorProto_TYPE_SINT32, descriptorpb.FieldDescriptorProto_TYPE_SINT64,
		descriptorpb.FieldDescriptorProto_TYPE_FIXED32, descriptorpb.FieldDescriptorProto_TYPE_FIXED64,
		descriptorpb.FieldDescriptorProto_TYPE_SFIXED32, descriptorpb.FieldDescriptorProto_TYPE_SFIXED64,
		descriptorpb.FieldDescriptorProto_TYPE_DOUBLE, descriptorpb.FieldDescriptorProto_TYPE_FLOAT:
		return "0"
	case descriptorpb.FieldDescriptorProto_TYPE_BOOL:
		return "False"
	case descriptorpb.FieldDescriptorProto_TYPE_BYTES:
		return `"c3ludGhldGlj"` // base64 of "synthetic"
	case descriptorpb.FieldDescriptorProto_TYPE_ENUM:
		return synthesizeEnumValue(field.GetTypeName(), st)
	case descriptorpb.FieldDescriptorProto_TYPE_MESSAGE:
		return synthesizeMessageBody(field.GetTypeName(), st, seen)
	default:
		return `"{{ faker.Word }}"`
	}
}

// synthesizeEnumValue returns the first enum value name as a quoted string,
// or a faker placeholder if the enum type can't be resolved.
func synthesizeEnumValue(typeName string, st *symbolTable) string {
	tn := normalizeTypeName(typeName)
	if enum, ok := st.enums[tn]; ok && len(enum.Value) > 0 {
		return fmt.Sprintf("%q", enum.Value[0].GetName())
	}
	return `"{{ faker.Word }}"`
}

// synthesizeMapValue produces a one-entry Starlark dict for a map field.
// msg is the map entry message (has key + value fields).
func synthesizeMapValue(msg *descriptorpb.DescriptorProto, st *symbolTable, seen map[string]bool) string {
	var keyExpr, valExpr string
	for _, f := range msg.Field {
		if f.GetName() == "key" {
			keyExpr = synthesizeScalar(f, st, seen)
		} else if f.GetName() == "value" {
			valExpr = synthesizeScalar(f, st, seen)
		}
	}
	if keyExpr == "" {
		keyExpr = `"{{ faker.Word }}"`
	}
	if valExpr == "" {
		valExpr = `"{{ faker.Word }}"`
	}
	return "{" + keyExpr + ": " + valExpr + "}"
}

// isMapEntry reports whether msg is a synthetic map_entry message.
func isMapEntry(msg *descriptorpb.DescriptorProto) bool {
	return msg.GetOptions().GetMapEntry()
}

// ---------------------------------------------------------------------------
// snake_case conversion
// ---------------------------------------------------------------------------

// toSnakeCase converts a CamelCase name to snake_case.
// e.g. CreateCharge -> create_charge, SayHello -> say_hello.
func toSnakeCase(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && isUpper(r) {
			// Add underscore before uppercase letter, but not if the previous
			// character was also uppercase (handles acronyms like ID -> id,
			// HTTPServer -> http_server).
			prev := rune(s[i-1])
			if !isUpper(prev) {
				b.WriteRune('_')
			}
		}
		b.WriteRune(toLower(r))
	}
	return b.String()
}

func isUpper(r rune) bool       { return r >= 'A' && r <= 'Z' }
func toLower(r rune) rune {
	if r >= 'A' && r <= 'Z' {
		return r + ('a' - 'A')
	}
	return r
}

// ---------------------------------------------------------------------------
// adapter.yaml merge
// ---------------------------------------------------------------------------

// protoManifest mirrors contrib.manifestData but adds the Grpc field.
// Unknown fields are dropped on re-marshal (same behaviour as MergeEndpoints).
type protoManifest struct {
	ID        string             `yaml:"id"`
	Name      string             `yaml:"name,omitempty"`
	Version   string             `yaml:"version,omitempty"`
	RealHosts []string           `yaml:"real_hosts,omitempty"`
	Endpoints []adapter.Endpoint `yaml:"endpoints,omitempty"`
	Resources []adapter.Resource `yaml:"resources,omitempty"`
	Identity  *adapter.Identity  `yaml:"identity,omitempty"`
	Rules     []rules.Rule       `yaml:"rules,omitempty"`
	Grpc      *adapter.GrpcSpec  `yaml:"grpc,omitempty"`
}

// mergeGrpcSection loads adapter.yaml from dir, adds or replaces the grpc:
// section, and writes it back. If adapter.yaml does not exist, a minimal
// manifest is created.
func mergeGrpcSection(dir, fullName, descRel, starRel string, svc *descriptorpb.ServiceDescriptorProto) error {
	manifestPath := filepath.Join(dir, "adapter.yaml")
	data, err := os.ReadFile(manifestPath)

	var m protoManifest
	if err == nil {
		if err := yaml.Unmarshal(data, &m); err != nil {
			return fmt.Errorf("proto: parse adapter.yaml: %w", err)
		}
	} else if os.IsNotExist(err) {
		m = protoManifest{
			ID:      filepath.Base(dir),
			Name:    fullName + " adapter",
			Version: "0.1.0",
		}
	} else {
		return fmt.Errorf("proto: read adapter.yaml: %w", err)
	}

	// Build the methods list.
	methods := make([]adapter.GrpcMethod, len(svc.Method))
	for i, method := range svc.Method {
		snake := toSnakeCase(method.GetName())
		methods[i] = adapter.GrpcMethod{
			Name:    method.GetName(),
			Handler: starRel + "#on_" + snake,
		}
	}

	m.Grpc = &adapter.GrpcSpec{
		Service:    fullName,
		Descriptor: descRel,
		Methods:    methods,
	}

	out, err := yaml.Marshal(&m)
	if err != nil {
		return fmt.Errorf("proto: marshal adapter.yaml: %w", err)
	}
	if err := os.WriteFile(manifestPath, out, 0o644); err != nil {
		return fmt.Errorf("proto: write adapter.yaml: %w", err)
	}
	return nil
}

