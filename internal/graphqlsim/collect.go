package graphqlsim

import (
	"github.com/vektah/gqlparser/v2/ast"
)

// collectedField represents a field that survived fragment expansion and
// directive evaluation. Multiple selections with the same response key
// (alias) are merged into one collectedField.
type collectedField struct {
	field      *ast.Field
	selections ast.SelectionSet // merged selections from all occurrences
}

// collectFields walks a SelectionSet, expanding fragments and evaluating
// @skip/@include directives. Fields with the same response key (alias or
// name) are merged.
func (ex *executor) collectFields(selections ast.SelectionSet, parentType *ast.Definition) []collectedField {
	var result []collectedField
	seen := map[string]int{} // response key → index in result
	ex.collectInto(selections, parentType, &result, seen, map[string]bool{})
	return result
}

// collectInto recursively collects fields from a selection set into result,
// expanding fragment spreads and inline fragments.
func (ex *executor) collectInto(selections ast.SelectionSet, parentType *ast.Definition, result *[]collectedField, seen map[string]int, visitedFragments map[string]bool) {
	for _, sel := range selections {
		switch s := sel.(type) {
		case *ast.Field:
			if ex.skipByDirective(s.Directives) {
				continue
			}
			ex.addField(result, seen, s)

		case *ast.InlineFragment:
			if ex.skipByDirective(s.Directives) {
				continue
			}
			// Type condition: must match parent type (or be abstract).
			if s.TypeCondition != "" {
				if !typeMatches(ex.schema, parentType, s.TypeCondition) {
					continue
				}
			}
			ex.collectInto(s.SelectionSet, parentType, result, seen, visitedFragments)

		case *ast.FragmentSpread:
			if ex.skipByDirective(s.Directives) {
				continue
			}
			if visitedFragments[s.Name] {
				continue // avoid cycles
			}
			frag := ex.doc.Fragments.ForName(s.Name)
			if frag == nil {
				continue
			}
			// Check type condition.
			if !typeMatches(ex.schema, parentType, frag.TypeCondition) {
				continue
			}
			visitedFragments[s.Name] = true
			ex.collectInto(frag.SelectionSet, parentType, result, seen, visitedFragments)
			delete(visitedFragments, s.Name) // allow reuse in sibling branches
		}
	}
}

// addField merges a field into the result set by response key.
func (ex *executor) addField(result *[]collectedField, seen map[string]int, f *ast.Field) {
	key := f.Alias
	if key == "" {
		key = f.Name
	}
	if idx, ok := seen[key]; ok {
		// Merge: append selections from this occurrence.
		cf := &(*result)[idx]
		cf.selections = append(cf.selections, f.SelectionSet...)
	} else {
		seen[key] = len(*result)
		// Copy the field so we can safely append merged selections.
		fc := *f
		fc.SelectionSet = append(ast.SelectionSet{}, f.SelectionSet...)
		*result = append(*result, collectedField{field: &fc, selections: fc.SelectionSet})
	}
}

// skipByDirective returns true if @skip(if:true) or @include(if:false) is
// present.
func (ex *executor) skipByDirective(dirs ast.DirectiveList) bool {
	if d := dirs.ForName("skip"); d != nil {
		args := d.ArgumentMap(ex.variables)
		if b, ok := args["if"].(bool); ok && b {
			return true
		}
	}
	if d := dirs.ForName("include"); d != nil {
		args := d.ArgumentMap(ex.variables)
		if b, ok := args["if"].(bool); ok && !b {
			return true
		}
	}
	return false
}

// typeMatches reports whether a concrete or abstract parent type is
// compatible with a fragment type condition. For object parent types, the
// condition must match exactly OR the condition must be an interface/union
// that the object implements. For abstract parent types, the condition
// must be applicable to at least one possible type.
func typeMatches(schema *ast.Schema, parentType *ast.Definition, condition string) bool {
	if parentType == nil {
		return false
	}
	if parentType.Name == condition {
		return true
	}

	condDef := schema.Types[condition]
	if condDef == nil {
		return false
	}

	// If the parent is an object and the condition is an interface/union
	// it implements.
	if parentType.Kind == ast.Object {
		for _, impl := range schema.GetImplements(parentType) {
			if impl.Name == condition {
				return true
			}
		}
		// Or the condition is a union that includes this object.
		if condDef.Kind == ast.Union {
			for _, pt := range schema.GetPossibleTypes(condDef) {
				if pt.Name == parentType.Name {
					return true
				}
			}
		}
	}

	return false
}
