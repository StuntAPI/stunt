package rules

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Evaluate applies rules in order. The first rule whose Match matches AND
// whose When gate passes produces the Decision. File-relative body refs
// resolve against baseDir.
func Evaluate(req Request, rules []Rule, rng *RNG, baseDir string) Decision {
	for _, r := range rules {
		if !r.Match.Matches(req) {
			continue
		}
		if r.When != nil && !rng.RollChance(r.When.Chance) {
			continue // gate failed; fall through to the next rule
		}
		return toDecision(r.Respond, baseDir)
	}
	return Decision{Matched: false}
}

func toDecision(resp Respond, baseDir string) Decision {
	d := Decision{Matched: true, Status: resp.Status, Headers: resp.Headers, LatencyMS: resp.LatencyMS}
	if resp.Behavior == "timeout" {
		d.Timeout = true
		if d.LatencyMS == 0 {
			d.LatencyMS = 30000
		}
		return d
	}
	if resp.Status == 0 {
		d.Status = 200
	}
	d.BodyBytes = bodyBytes(resp.Body, baseDir)
	return d
}

func bodyBytes(b *Body, baseDir string) []byte {
	if b == nil {
		return nil
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
