package sharedcapacity

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// Payload model for the pinned 32-GiB type-8, eight-base-tree layout, default
// rows_to_discard=2. Not a production envelope: allocation/metadata, retries,
// copies and the full caller protocol must be accounted separately.
func reviewedPayloadBound() (peak, resident int64) {
	baseLeaves := modelSector / 8 / 32
	tree := func(leaves int64) int64 { return (8*leaves - 1) / 7 * 32 }
	fullBase := tree(baseLeaves)
	cacheBase := tree(baseLeaves / 512)
	labels, treeD, sealed := 11*modelSector, 2*modelSector-32, modelSector
	treeC := 8 * fullBase
	resident = labels + treeD + sealed + treeC + 8*cacheBase
	// CPU builds base trees sequentially; front_truncate copies within the
	// same file. Previously completed base trees retain only their cache.
	peak = labels + treeD + sealed + treeC + fullBase + 7*cacheBase
	return
}

func TestReviewedPayloadArithmeticAndOldSensitivity(t *testing.T) {
	peak, resident := reviewedPayloadBound()
	t.Logf("source payload only: peak=%d resident=%d six-peak+margin=%d", peak, resident, 6*peak+fixturePolicy.Margin)
	if peak <= resident || 6*peak+fixturePolicy.Margin > 3200000000000 {
		t.Fatal(peak, resident)
	}
	a, _ := fixture(t, 3200000000000)
	for i := 0; i < 2; i++ {
		key := fmt.Sprint("resident", i)
		token, _, err := a.Reserve(context.Background(), key, "boot", modelEnvelope)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Start(context.Background(), key, token); err != nil {
			t.Fatal(err)
		}
		if err := a.Returned(context.Background(), key, token); err != nil {
			t.Fatal(err)
		}
	}
	accepted := 0
	for i := 0; i < 4; i++ {
		_, _, err := a.Reserve(context.Background(), fmt.Sprint(i), "boot", modelEnvelope)
		if err == nil {
			accepted++
		} else if !errors.Is(err, ErrCapacity) {
			t.Fatal(err)
		}
	}
	if accepted != 3 {
		t.Fatal("old 16*S sensitivity changed", accepted)
	}
}

type simulatedSector struct {
	key, token             string
	role, stage, remaining int
	bytes                  int64
}

// Virtual 15-second clock; no native timing or production measurement. Stage
// duration is an explicit sensitivity input, not a claim inferred from source.
// There is no batch drain barrier: each SDR completion releases one execution
// slot immediately, while every previous sector continues downstream.
func TestContinuousRefillModel(t *testing.T) {
	for _, dual := range []bool{false, true} {
		for _, stall := range []string{"none", "TreeD", "TreeRC", "PreCommit", "WaitSeed", "PoRep", "Finalize", "MoveStorage", "combined"} {
			t.Run(fmt.Sprintf("dual=%t/stall=%s", dual, stall), func(t *testing.T) {
				capacity, roles, slots, cadence := int64(3200000000000), 1, 4, 176 // 44m model cadence
				if dual {
					capacity, roles, slots, cadence = 12800000000000, 2, 6, 103
				} // 25m45s
				peak, resident := reviewedPayloadBound()
				// Small-file payload allowance is a fixture input, NOT verified
				// native/FS metadata headroom. Keep it explicit in the output.
				envelope := peak + 64<<20
				a, _ := fixture(t, capacity)
				sectors := map[string]*simulatedSector{}
				a.sample = func(context.Context, State) (Sample, error) {
					v := Sample{Identity: fixtureID, Free: capacity, Credit: map[string]int64{}}
					for key, sector := range sectors {
						v.Free -= sector.bytes
						v.Credit[key] = sector.bytes
					}
					return v, nil
				}
				stages := []string{"SDR", "TreeD", "TreeRC", "PreCommit", "WaitSeed", "PoRep", "Finalize", "MoveStorage"}
				durations := []int{slots*cadence - 2, 8, 8, 8, 300, 8, 4, 4}
				active, starts, completions, next, high := make([]int, roles), make([]int, roles), make([]int, roles), make([]int, roles), make([]int, roles)
				waits, overlap, residentHigh, serial := 0, 0, 0, 0
				var physicalPeak int64
				observe := func() {
					var used int64
					for _, s := range sectors {
						used += s.bytes
					}
					physicalPeak = max(physicalPeak, used)
					if used+fixturePolicy.Margin > capacity {
						t.Fatal("model physical envelope overrun", used)
					}
				}
				fullTicks := make([]int, roles)
				ctx := context.Background()
				sequence := uint64(1)
				apply := func(op, key, token string, bytes int64, proof func(Entry) error) (Outcome, error) {
					r := Request{Client: "continuous-model", Sequence: sequence, Operation: op, Sector: key, Token: token, Boot: "boot", Envelope: bytes}
					o, err := a.Apply(ctx, r, proof)
					if err == nil {
						sequence++
					}
					return o, err
				}
				for tick := 0; tick < 24*60*4; tick++ {
					for key, s := range sectors {
						blocked := tick >= 2400 && tick < 4200 && s.stage > 0 && (stall == "combined" || stall == stages[s.stage])
						if blocked {
							continue
						}
						s.remaining--
						if s.remaining > 0 {
							continue
						}
						if s.stage == 0 {
							active[s.role]--
							completions[s.role]++
							if _, err := apply("returned", key, s.token, 0, nil); err != nil {
								t.Fatal(err)
							}
						}
						s.stage++
						if s.stage == len(stages) {
							if _, err := apply("reconcile", key, s.token, 0, func(Entry) error { delete(sectors, key); return nil }); err != nil {
								t.Fatal(err)
							}
							continue
						}
						s.remaining = durations[s.stage]
						switch s.stage {
						case 1:
							s.bytes = 13*modelSector - 32
						case 2:
							s.bytes = peak
						case 6, 7:
							s.bytes = 2 * modelSector // sealed/unsealed, rounded residual model
						default:
							s.bytes = resident
						}
						observe()
					}
					residents := 0
					var used int64
					for _, s := range sectors {
						used += s.bytes
						if s.stage > 0 {
							residents++
						}
					}
					physicalPeak = max(physicalPeak, used)
					residentHigh = max(residentHigh, residents)
					if used+fixturePolicy.Margin > capacity {
						t.Fatal("model physical envelope overrun", used)
					}
					// Alternate polling order; this is an explicit fairness input,
					// not a guarantee supplied by flock or production scheduling.
					for offset := 0; offset < roles; offset++ {
						r := (tick + offset) % roles
						if active[r] == slots || tick < next[r] {
							continue
						}
						key := fmt.Sprint("sector-", serial)
						serial++
						grant, err := apply("reserve", key, "", envelope, nil)
						if errors.Is(err, ErrCapacity) {
							waits++
							continue
						}
						if err != nil {
							t.Fatal(err)
						}
						token := grant.Entry.Token
						if _, err := apply("start", key, token, 0, nil); err != nil {
							t.Fatal(err)
						}
						sectors[key] = &simulatedSector{key: key, token: token, role: r, remaining: durations[0], bytes: 11 * modelSector}
						observe()
						active[r]++
						starts[r]++
						next[r] = tick + cadence
						high[r] = max(high[r], active[r])
						if residents > 0 {
							overlap++
						}
					}
					for r := range roles {
						if active[r] == slots {
							fullTicks[r]++
						}
						if stall == "none" && tick >= slots*cadence && active[r] < slots-1 {
							t.Fatal("steady state lost slots", tick, active)
						}
					}
				}
				for r := range roles {
					if high[r] != slots || starts[r] < 12 || completions[r] < 10 || active[r] < slots-1 {
						t.Fatalf("no sustained refill: active=%v starts=%v completed=%v high=%v", active, starts, completions, high)
					}
				}
				if overlap < 10 || residentHigh < 2 {
					t.Fatal("batch-only fixture", overlap, residentHigh)
				}
				if stall == "none" && waits != 0 {
					t.Fatal("healthy modeled cadence denied", waits)
				}
				if stall != "none" && waits == 0 {
					t.Fatal("stall failed to exercise backpressure")
				}
				t.Logf("virtual seconds=86400 roles=%d slots=%d starts=%v completed=%v full_slot_ticks=%v capacity_wait_polls=%d overlap_refills=%d resident_peak=%d materialized_peak=%d envelope=%d", roles, slots, starts, completions, fullTicks, waits, overlap, residentHigh, physicalPeak, envelope)
			})
		}
	}
}
