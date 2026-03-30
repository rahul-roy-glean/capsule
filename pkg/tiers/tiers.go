package tiers

import "fmt"

// CPUOversubscriptionFactor is the ratio of effective CPU to actual vCPUs.
// Firecracker VMs rarely saturate all cores simultaneously, so we allow
// 4:1 oversubscription (each host core serves 2 guest vCPUs).
const CPUOversubscriptionFactor = 0.25

// Tier defines a VM resource tier baked into a snapshot.
type Tier struct {
	Name     string
	VCPUs    int // integer vCPUs baked into the snapshot
	MemoryMB int // memory baked into the snapshot
}

// All is the set of supported tiers keyed by name.
var All = map[string]Tier{
	"xs": {Name: "xs", VCPUs: 1, MemoryMB: 512},
	"s":  {Name: "s", VCPUs: 1, MemoryMB: 1024},
	"m":  {Name: "m", VCPUs: 4, MemoryMB: 4096},
	"l":  {Name: "l", VCPUs: 8, MemoryMB: 8192},
	"xl": {Name: "xl", VCPUs: 16, MemoryMB: 16384},
}

// DefaultTier is the tier used when none is specified.
const DefaultTier = "m"

// Lookup returns the tier for the given name, or an error if invalid.
func Lookup(name string) (Tier, error) {
	t, ok := All[name]
	if !ok {
		return Tier{}, fmt.Errorf("unknown tier %q", name)
	}
	return t, nil
}

// EffectiveCPUMillicores returns the CPU millicores consumed for scheduling
// purposes, applying the oversubscription factor.
func EffectiveCPUMillicores(t Tier) int {
	return int(float64(t.VCPUs*1000) * CPUOversubscriptionFactor)
}
