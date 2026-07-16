// Package validator validates a Go value (a parsed JSON map or any other
// JSON-compatible type) against a JSON-Schema document. It is used for
// request/response conformance checking: adapters compile a schema once,
// then validate many documents against it.
//
// Only JSON-Schema (draft 4 through 2020-12) is supported. OpenAPI-specific
// extensions and $ref resolution across files are OUT OF SCOPE — schemas
// must be self-contained single documents.
package validator

import (
	"errors"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// ValidationError describes a single schema violation at a specific path
// within the validated document.
type ValidationError struct {
	// Path is the JSON-pointer-style location of the offending value
	// within the document (e.g. "" for the root, "/age" for a property).
	Path string

	// Message is a human-readable description of the violation.
	Message string
}

// String returns a readable representation for debugging and logging.
func (e ValidationError) String() string {
	if e.Path == "" {
		return e.Message
	}
	return e.Path + ": " + e.Message
}

// Schema is a compiled JSON-Schema document. It is safe for concurrent use
// once compiled.
type Schema struct {
	compiled *jsonschema.Schema
}

// Validator compiles JSON-Schema documents into reusable *Schema objects.
type Validator struct {
	compiler *jsonschema.Compiler
}

// NewValidator creates a Validator with a fresh schema compiler.
func NewValidator() *Validator {
	return &Validator{compiler: jsonschema.NewCompiler()}
}

// Compile parses schemaJSON (a JSON-Schema document) and returns a compiled
// Schema. Returns an error if the JSON is malformed or the schema is invalid.
func (v *Validator) Compile(schemaJSON []byte) (*Schema, error) {
	url := "schema.json"
	if err := v.compiler.AddResource(url, strings.NewReader(string(schemaJSON))); err != nil {
		return nil, fmt.Errorf("validator: add resource: %w", err)
	}
	compiled, err := v.compiler.Compile(url)
	if err != nil {
		return nil, fmt.Errorf("validator: compile schema: %w", unwrapSchemaErr(err))
	}
	return &Schema{compiled: compiled}, nil
}

// Validate checks v against the compiled schema. It returns a list of
// ValidationError describing every violation (empty list = valid). The
// second return value is non-nil only for unexpected internal errors, not
// for validation failures.
func (s *Schema) Validate(v any) ([]ValidationError, error) {
	err := s.compiled.Validate(v)
	if err == nil {
		return nil, nil
	}

	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		// Not a validation error — something unexpected happened.
		return nil, fmt.Errorf("validator: validate: %w", err)
	}

	return flattenErrors(ve), nil
}

// flattenErrors walks the jsonschema ValidationError tree (which is nested
// via Causes) and collects all leaf nodes that have a non-empty message.
// If a node has no message but has causes, its causes are descended into.
func flattenErrors(ve *jsonschema.ValidationError) []ValidationError {
	var out []ValidationError
	walk(ve, &out)
	return out
}

func walk(ve *jsonschema.ValidationError, out *[]ValidationError) {
	if len(ve.Causes) == 0 {
		// Leaf node: record it if it has a message.
		if ve.Message != "" {
			*out = append(*out, ValidationError{
				Path:    ve.InstanceLocation,
				Message: ve.Message,
			})
		}
		return
	}
	// Non-leaf: descend into causes. If this node also has a message,
	// record it first.
	if ve.Message != "" {
		*out = append(*out, ValidationError{
			Path:    ve.InstanceLocation,
			Message: ve.Message,
		})
	}
	for _, cause := range ve.Causes {
		walk(cause, out)
	}
}

// unwrapSchemaErr returns the underlying error from a *jsonschema.SchemaError,
// if applicable, otherwise returns the original error. This gives callers a
// cleaner error message on compilation failures.
func unwrapSchemaErr(err error) error {
	var se *jsonschema.SchemaError
	if errors.As(err, &se) {
		return se.Err
	}
	return err
}
