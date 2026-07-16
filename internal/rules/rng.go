package rules

import "math/rand"

// RNG is a deterministic 0..100 source used by when.chance.
type RNG struct {
	r *rand.Rand
}

func NewRNG(seed int64) *RNG {
	return &RNG{r: rand.New(rand.NewSource(seed))}
}

// Percent returns an integer in [1,100].
func (r *RNG) Percent() int {
	return r.r.Intn(100) + 1
}

// RollChance returns true with probability chance/100 (chance in 0..100).
func (r *RNG) RollChance(chance int) bool {
	if chance <= 0 {
		return false
	}
	return r.Percent() <= chance
}
