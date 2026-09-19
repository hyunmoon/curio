package sharedcapacity

import (
	"math"
	"testing"
)

func TestActualExtentFilter(t *testing.T) {
	for _, flags := range []uint32{0, 1, 0x800, 0x801} {
		credit, end, err := extentCredit(fileExtent{Logical: 4096, Length: 8192, Flags: flags}, 4096)
		if err != nil || credit != 8192 || end != 12288 {
			t.Fatal(credit, end, err)
		}
	}
	for _, flag := range []uint32{2, 4, 8, 0x80, 0x100, 0x200, 0x400, 0x1000, 0x2000, 0x4000} {
		credit, _, err := extentCredit(fileExtent{Length: 8192, Flags: flag | 1}, 0)
		if err != nil || credit != 0 {
			t.Fatal("unproven extent received credit", flag, credit, err)
		}
	}
	for _, e := range []fileExtent{{Length: 0}, {Logical: 1, Length: 10}, {Logical: math.MaxUint64, Length: 1}, {Logical: 4096, Length: math.MaxInt64 + 1}} {
		if _, _, err := extentCredit(e, 4096); err == nil {
			t.Fatal("invalid extent accepted", e)
		}
	}
}
