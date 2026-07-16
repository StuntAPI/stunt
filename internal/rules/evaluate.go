package rules

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
			if !rng.RollChance(r.When.Chance) {
				continue
			}
			if r.When.Expr != "" && !evalExpr(r.When.Expr, req) {
				continue
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
		p := b.File
		if !filepath.IsAbs(p) {
			p = filepath.Join(baseDir, p)
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

// evalExpr is a temporary stub; replaced in Task 4.
func evalExpr(expr string, req Request) bool { return true }
