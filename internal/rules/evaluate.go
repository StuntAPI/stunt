package rules

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/expr-lang/expr"
	"github.com/stunt-adapters/stunt/internal/pathutil"
)

// Evaluate applies rules in order. The first rule whose Match matches AND
// whose When gate passes produces the Decision. File-relative and templated
// body refs resolve using req, faker, and baseDir.
func Evaluate(req Request, rules []Rule, rng *RNG, faker *Faker, baseDir string) Decision {
	for _, r := range rules {
		if !r.Match.Matches(req) {
			continue
		}
		if r.When != nil {
			if r.When.Expr != "" {
				// Expr mode: chance is optional (0 = no chance gate).
				if r.When.Chance > 0 && !rng.RollChance(r.When.Chance) {
					continue
				}
				if !evalExpr(r.When.Expr, req) {
					continue
				}
			} else {
				// Chance-only mode: 0% = never fires.
				if !rng.RollChance(r.When.Chance) {
					continue
				}
			}
		}
		return toDecision(r.Respond, baseDir, req, faker)
	}
	return Decision{Matched: false}
}

func toDecision(resp Respond, baseDir string, req Request, fk *Faker) Decision {
	d := Decision{Matched: true, Status: resp.Status, Headers: resp.Headers, LatencyMS: resp.LatencyMS}
	if resp.Behavior == "timeout" {
		d.Timeout = true
		if d.LatencyMS == 0 {
			d.LatencyMS = 30000
		}
		return d
	}
	if d.Status == 0 {
		d.Status = 200
	}
	d.BodyBytes = bodyBytes(resp.Body, baseDir, req, fk)
	return d
}

func bodyBytes(b *Body, baseDir string, req Request, fk *Faker) []byte {
	if b == nil {
		return nil
	}
	if b.Template != "" {
		out, err := renderTemplate(b.Template, req, fk)
		if err != nil {
			return []byte(fmt.Sprintf("stunt: template error: %v", err))
		}
		return out
	}
	if b.File != "" {
		// Security: validate the file path stays within baseDir to prevent
		// path-traversal attacks (../../etc/passwd). An adapter must never
		// be able to read files outside its own directory.
		p, err := pathutil.ContainedPath(baseDir, b.File)
		if err != nil {
			return []byte(fmt.Sprintf("stunt: body file path rejected: %v", err))
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return []byte(fmt.Sprintf("stunt: body file error: %v", err))
		}
		return data
	}
	if b.Inline != nil {
		data, err := json.Marshal(b.Inline)
		if err != nil {
			return []byte(fmt.Sprintf("stunt: inline marshal error: %v", err))
		}
		return data
	}
	return nil
}

// evalExpr evaluates a boolean expression over a request.* environment.
// request = { method, path, headers, body } (body parsed from JSON).
func evalExpr(e string, req Request) bool {
	env := map[string]any{
		"request": map[string]any{
			"method":  req.Method,
			"path":    req.Path,
			"headers": req.Headers,
			"body":    parseJSON(req.Body),
		},
	}
	out, err := expr.Eval(e, env)
	if err != nil {
		return false
	}
	b, _ := out.(bool)
	return b
}
