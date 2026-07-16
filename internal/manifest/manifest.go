package manifest

import (
	"os"

	"github.com/stunt-adapters/stunt/internal/rules"
	"gopkg.in/yaml.v3"
)

type Network struct {
	Mode     string `yaml:"mode"`      // "port" (plan 1); "subdomain" later
	BasePort int    `yaml:"base_port"` // sequential ports start here
}

type Service struct {
	Adapter string       `yaml:"adapter"` // path to an adapter dir (optional)
	Rules   []rules.Rule `yaml:"rules"`
}

type Manifest struct {
	Path     string             `yaml:"-"`
	Version  int                `yaml:"version"`
	RNGSeed  int64              `yaml:"rng_seed"`
	Network  Network            `yaml:"network"`
	Services map[string]Service `yaml:"services"`
}

// Load reads and parses a stunt.yaml from path.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m := &Manifest{Path: path}
	if err := yaml.Unmarshal(data, m); err != nil {
		return nil, err
	}
	return m, nil
}
