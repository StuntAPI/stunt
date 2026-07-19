package graphqlsim

import (
	"github.com/vektah/gqlparser/v2/ast"
)

// analyzeOperation walks the operation (expanding fragments) to compute the
// maximum selection depth and total field count. These are used for DoS
// limiting BEFORE execution begins.
//
// Depth is measured as the number of nesting levels of selections. A flat
// query { a b } has depth 1. { user { name } } has depth 2.
//
// Field count is the total number of fields after fragment expansion
// (counting each occurrence, including inline/fragment fields).
func analyzeOperation(op *ast.OperationDefinition, fragments ast.FragmentDefinitionList) (depth, fields int) {
	fragMap := make(map[string]*ast.FragmentDefinition, len(fragments))
	for i := range fragments {
		fragMap[fragments[i].Name] = fragments[i]
	}
	d, f := walkDepth(op.SelectionSet, fragMap, map[string]bool{})
	return d, f
}

// walkDepth returns (maxDepth, totalFields) for a selection set.
func walkDepth(selections ast.SelectionSet, fragments map[string]*ast.FragmentDefinition, visited map[string]bool) (int, int) {
	maxDepth := 0
	totalFields := 0

	for _, sel := range selections {
		switch s := sel.(type) {
		case *ast.Field:
			totalFields++
			if len(s.SelectionSet) > 0 {
				d, f := walkDepth(s.SelectionSet, fragments, visited)
				if d > maxDepth {
					maxDepth = d
				}
				totalFields += f
			}

		case *ast.InlineFragment:
			if len(s.SelectionSet) > 0 {
				d, f := walkDepth(s.SelectionSet, fragments, visited)
				if d > maxDepth {
					maxDepth = d
				}
				totalFields += f
			}

		case *ast.FragmentSpread:
			if visited[s.Name] {
				continue
			}
			frag := fragments[s.Name]
			if frag == nil {
				continue
			}
			visited[s.Name] = true
			d, f := walkDepth(frag.SelectionSet, fragments, visited)
			delete(visited, s.Name)
			if d > maxDepth {
				maxDepth = d
			}
			totalFields += f
		}
	}

	return maxDepth + 1, totalFields
}
