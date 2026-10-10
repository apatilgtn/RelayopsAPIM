package graphql

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

// maxCost caps computed costs so multiplied list sizes cannot overflow.
const maxCost = 1 << 40

// DefaultListSizeArguments are the arguments whose integer value multiplies
// the cost of a field's children (a page of N items costs N times its items).
var DefaultListSizeArguments = []string{"first", "last", "limit", "pageSize"}

// AnalyzeOperation parses a request's document and measures the operation
// that would run: the one named operationName, or the only one in the
// document. Depth and cost follow fragment spreads; aliases, introspection
// and the operation type come from the syntax tree, not the raw text.
func AnalyzeOperation(query, operationName string, variables map[string]any, listArgs []string) (AnalysisResult, error) {
	res := AnalysisResult{Hash: QueryHash(query)}
	doc, err := parser.ParseQuery(&ast.Source{Input: query})
	if err != nil {
		return res, fmt.Errorf("GraphQL syntax error: %s", err.Error())
	}
	op, err := selectOperation(doc, operationName)
	if err != nil {
		return res, err
	}
	res.OperationType = string(op.Operation)
	res.OperationName = op.Name
	if len(listArgs) == 0 {
		listArgs = DefaultListSizeArguments
	}
	w := &walker{doc: doc, vars: variables, listArgs: listArgs, memo: map[string]measure{}, onPath: map[string]bool{}}
	m, err := w.selectionSet(op.SelectionSet)
	if err != nil {
		return res, err
	}
	res.Depth, res.Cost, res.Aliases, res.IsIntrospection = m.depth, int(min(m.cost, maxCost)), m.aliases, m.introspection
	if res.Cost == 0 {
		res.Cost = 1
	}
	for _, sel := range op.SelectionSet {
		if f, ok := sel.(*ast.Field); ok {
			res.TopLevelFields = append(res.TopLevelFields, f.Name)
		}
	}
	return res, nil
}

// QueryHash is the SHA-256 of the query text, as used by persisted-query
// (APQ) clients.
func QueryHash(query string) string {
	sum := sha256.Sum256([]byte(query))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func selectOperation(doc *ast.QueryDocument, name string) (*ast.OperationDefinition, error) {
	if len(doc.Operations) == 0 {
		return nil, errors.New("GraphQL document has no operation")
	}
	if name != "" {
		if op := doc.Operations.ForName(name); op != nil {
			return op, nil
		}
		return nil, fmt.Errorf("operation %q is not defined in the document", name)
	}
	if len(doc.Operations) > 1 {
		return nil, errors.New("document defines several operations; operationName is required")
	}
	return doc.Operations[0], nil
}

type measure struct {
	depth, aliases int
	cost           int64
	introspection  bool
}

type walker struct {
	doc      *ast.QueryDocument
	vars     map[string]any
	listArgs []string
	memo     map[string]measure // per fragment: its selection set is measured once
	onPath   map[string]bool    // fragments being expanded (cycle detection)
}

func (w *walker) selectionSet(set ast.SelectionSet) (measure, error) {
	var out measure
	merge := func(m measure, depthOffset int) {
		out.depth = max(out.depth, m.depth+depthOffset)
		out.cost = min(out.cost+m.cost, maxCost)
		out.aliases += m.aliases
		out.introspection = out.introspection || m.introspection
	}
	for _, sel := range set {
		switch s := sel.(type) {
		case *ast.Field:
			child, err := w.selectionSet(s.SelectionSet)
			if err != nil {
				return out, err
			}
			fm := measure{depth: child.depth + 1, aliases: child.aliases, introspection: child.introspection}
			if s.Alias != "" && s.Alias != s.Name {
				fm.aliases++
			}
			if s.Name == "__schema" || s.Name == "__type" {
				fm.introspection = true
			}
			fm.cost = min(1+w.multiplier(s)*child.cost, maxCost)
			merge(fm, 0)
		case *ast.InlineFragment:
			m, err := w.selectionSet(s.SelectionSet)
			if err != nil {
				return out, err
			}
			merge(m, 0)
		case *ast.FragmentSpread:
			m, err := w.fragment(s.Name)
			if err != nil {
				return out, err
			}
			merge(m, 0)
		}
	}
	return out, nil
}

func (w *walker) fragment(name string) (measure, error) {
	if m, ok := w.memo[name]; ok {
		return m, nil
	}
	if w.onPath[name] {
		return measure{}, fmt.Errorf("fragment %q spreads itself (cycle)", name)
	}
	def := w.doc.Fragments.ForName(name)
	if def == nil {
		return measure{}, fmt.Errorf("fragment %q is not defined", name)
	}
	w.onPath[name] = true
	m, err := w.selectionSet(def.SelectionSet)
	delete(w.onPath, name)
	if err == nil {
		w.memo[name] = m
	}
	return m, err
}

// multiplier is the list size a field asks for (first: 50 → 50), or 1.
func (w *walker) multiplier(f *ast.Field) int64 {
	for _, a := range f.Arguments {
		if !containsFold(w.listArgs, a.Name) || a.Value == nil {
			continue
		}
		var raw string
		switch a.Value.Kind {
		case ast.IntValue:
			raw = a.Value.Raw
		case ast.Variable:
			if v, ok := w.vars[a.Value.Raw]; ok {
				raw = fmt.Sprint(v)
			}
		}
		if n, err := strconv.ParseFloat(raw, 64); err == nil && n > 1 {
			return int64(min(n, 1e6))
		}
	}
	return 1
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

// LoadSchema parses an SDL schema for request validation.
func LoadSchema(sdl string) (*ast.Schema, error) {
	s, err := gqlparser.LoadSchema(&ast.Source{Name: "schema.graphql", Input: sdl})
	if err != nil {
		return nil, fmt.Errorf("invalid GraphQL schema: %s", err.Error())
	}
	return s, nil
}

// ValidateAgainstSchema checks a document with the GraphQL specification's
// validation rules: fields exist on their types, arguments and variables
// are typed correctly, fragments apply, and so on.
func ValidateAgainstSchema(schema *ast.Schema, query string) []string {
	_, errs := gqlparser.LoadQuery(schema, query)
	out := make([]string, 0, len(errs))
	for _, e := range errs {
		out = append(out, e.Message)
	}
	return out
}
