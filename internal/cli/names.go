package cli

import (
	"fmt"
	"math/rand"
	"time"
)

var runNameAdjectives = []string{
	"brave", "calm", "clever", "eager", "fuzzy", "gentle", "happy", "jolly",
	"keen", "lively", "merry", "nimble", "proud", "quiet", "rapid", "swift",
}

var runNameNouns = []string{
	"falcon", "otter", "badger", "heron", "lynx", "panda", "raven", "sparrow",
	"tiger", "viper", "wombat", "yak", "zebra", "gecko", "ibis", "jackal",
}

// GenerateVMName returns a Docker-style adjective-noun name (e.g.
// "brave-falcon") not present in existing. Retries a bounded number of
// times to dodge collisions in the 256-combo space, then falls back to a
// timestamp-suffixed name so it always terminates.
func GenerateVMName(existing map[string]bool) string {
	for i := 0; i < 30; i++ {
		candidate := runNameAdjectives[rand.Intn(len(runNameAdjectives))] + "-" + runNameNouns[rand.Intn(len(runNameNouns))]
		if !existing[candidate] {
			return candidate
		}
	}
	return fmt.Sprintf("run-%d", time.Now().UnixNano())
}
