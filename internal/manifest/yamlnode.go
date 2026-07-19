package manifest

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// AddServiceNode surgically adds or updates a single service in the manifest
// YAML file at path, preserving comments, indentation, and flow/block style
// of all existing content. It uses yaml.Node round-trip: the file is parsed
// as a node tree, only the target service node is inserted under services:,
// and the same tree is re-serialized.
//
// The service's adapter field is set to adapterSpec. Existing rules and
// config on the service (if any) are preserved.
func AddServiceNode(path string, name string, adapterSpec string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("manifest: parse for surgical edit: %w", err)
	}

	docNode := getDocMapping(&root)

	// Find or create the "services" mapping.
	servicesNode := findOrCreateMappingEntry(docNode, "services")
	if servicesNode.Kind != yaml.MappingNode {
		servicesNode.Kind = yaml.MappingNode
		servicesNode.Tag = "!!map"
	}

	// Find or create the service name key under services.
	svcNode := findOrCreateMappingEntry(servicesNode, name)
	if svcNode.Kind != yaml.MappingNode {
		svcNode.Kind = yaml.MappingNode
		svcNode.Tag = "!!map"
	}

	// Set the adapter field on the service node.
	setMappingEntry(svcNode, "adapter", adapterSpec)

	// Re-serialize the node tree.
	out, err := marshalNode(&root)
	if err != nil {
		return fmt.Errorf("manifest: marshal after surgical edit: %w", err)
	}

	return atomicWrite(path, out)
}

// RemoveServiceNode surgically removes a single service from the manifest
// YAML file at path, preserving all other content. Uses yaml.Node round-trip.
func RemoveServiceNode(path string, name string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("manifest: parse for surgical edit: %w", err)
	}

	docNode := getDocMapping(&root)

	// Find the "services" mapping.
	servicesNode := findMappingEntry(docNode, "services")
	if servicesNode == nil {
		return nil // no services section — nothing to remove
	}

	// Remove the service key-value pair from the services mapping.
	removeMappingEntry(servicesNode, name)

	// Re-serialize.
	out, err := marshalNode(&root)
	if err != nil {
		return fmt.Errorf("manifest: marshal after surgical edit: %w", err)
	}

	return atomicWrite(path, out)
}

// getDocMapping returns the root mapping node of a YAML document. If the
// node is a document node, it returns Content[0]; otherwise it returns the
// node itself (for convenience when the caller already has a mapping).
func getDocMapping(root *yaml.Node) *yaml.Node {
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		return root.Content[0]
	}
	return root
}

// findMappingEntry returns the value node for a key in a mapping node, or
// nil if not found.
func findMappingEntry(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// findOrCreateMappingEntry returns the value node for a key in a mapping
// node, creating a new key-value pair if the key doesn't exist. The new
// value node is an empty node (Kind 0) that the caller can populate.
func findOrCreateMappingEntry(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	valNode := &yaml.Node{}
	mapping.Content = append(mapping.Content, keyNode, valNode)
	return valNode
}

// setMappingEntry sets a key-value pair in a mapping node, creating the pair
// if it doesn't exist, or updating the value if it does.
func setMappingEntry(mapping *yaml.Node, key, value string) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1].Kind = yaml.ScalarNode
			mapping.Content[i+1].Tag = "!!str"
			mapping.Content[i+1].Value = value
			return
		}
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	valNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
	mapping.Content = append(mapping.Content, keyNode, valNode)
}

// removeMappingEntry removes a key-value pair from a mapping node. If the
// key doesn't exist, the mapping is unchanged.
func removeMappingEntry(mapping *yaml.Node, key string) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return
		}
	}
}

// marshalNode serializes a yaml.Node using 2-space indentation (matching
// the default convention used by most YAML files and the convention in
// stunt.yaml). yaml.Marshal defaults to 4 spaces, which would change the
// indentation of every manifest on edit.
func marshalNode(root *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// atomicWrite writes data to path atomically using a temp file + rename.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, ".stunt-*.tmp")
	if err != nil {
		return fmt.Errorf("manifest: create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if _, statErr := os.Stat(tmpName); statErr == nil {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("manifest: write temp file %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("manifest: close temp file %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("manifest: chmod temp file %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("manifest: rename %s → %s: %w", tmpName, path, err)
	}
	return nil
}
