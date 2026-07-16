package rules

import fakeit "github.com/brianvoe/gofakeit/v6"

// Faker wraps a seeded gofakeit so templates and generators are deterministic.
// Exposed to templates as the {{ faker.X }} funcs.
type Faker struct {
	f *fakeit.Faker
}

// NewFaker creates a deterministically-seeded faker.
func NewFaker(seed int64) *Faker {
	return &Faker{f: fakeit.New(seed)}
}

// ID returns "<prefix>_<uuid>", or a bare uuid when prefix is empty.
func (fk *Faker) ID(prefix string) string {
	if prefix == "" {
		return fk.f.UUID()
	}
	return prefix + "_" + fk.f.UUID()
}

func (fk *Faker) Email() string    { return fk.f.Email() }
func (fk *Faker) Name() string     { return fk.f.Name() }
func (fk *Faker) Username() string { return fk.f.Username() }
func (fk *Faker) Word() string     { return fk.f.Noun() }

// Int returns a random int in [min, max].
func (fk *Faker) Int(min, max int) int { return fk.f.IntRange(min, max) }
