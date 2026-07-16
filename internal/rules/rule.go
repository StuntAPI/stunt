package rules

// Match describes when a rule applies.
type Match struct {
	Method  string            `yaml:"method,omitempty"`
	Path    string            `yaml:"path,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty"`
}

// When gates whether a matched rule actually fires.
type When struct {
	Chance int    `yaml:"chance,omitempty"` // percent 0..100
	Expr   string `yaml:"expr,omitempty"`   // boolean expression over request.*
}

// Body selects how a response body is produced.
// Plan 1: inline literal, static file. Plan 2 adds: template (rendered text).
type Body struct {
	Inline   any    `yaml:"inline,omitempty"`
	File     string `yaml:"file,omitempty"`
	Template string `yaml:"template,omitempty"`
}

// Respond describes what to return when a rule fires.
type Respond struct {
	Status    int               `yaml:"status,omitempty"`
	Headers   map[string]string `yaml:"headers,omitempty"`
	Body      *Body             `yaml:"body,omitempty"`
	LatencyMS int               `yaml:"latency_ms,omitempty"`
	Behavior  string            `yaml:"behavior,omitempty"` // "timeout"
}

// Rule is one entry in a service's ordered rule list.
type Rule struct {
	Name    string  `yaml:"name,omitempty"`
	Match   Match   `yaml:"match"`
	When    *When   `yaml:"when,omitempty"`
	Respond Respond `yaml:"respond"`
}

// Decision is the engine-facing result of evaluating rules for a request.
type Decision struct {
	Matched   bool
	Status    int
	Headers   map[string]string
	BodyBytes []byte
	LatencyMS int
	Timeout   bool
}
