package sharedcapacity

import "math"

// Linux FIEMAP UAPI representation; kept platform-independent so validation of
// kernel observations is exercised by the host test suite as well.
type fileExtent struct {
	Logical, Physical, Length uint64
	Reserved64                [2]uint64
	Flags                     uint32
	Reserved                  [3]uint32
}

func extentCredit(e fileExtent, start uint64) (int64, uint64, error) {
	if e.Length == 0 || e.Logical < start || e.Length > math.MaxUint64-e.Logical {
		return 0, start, ErrIdentity
	}
	end := e.Logical + e.Length
	// LAST and UNWRITTEN are the only known safe allocation flags. In
	// particular, sparse length, delalloc and shared/COW are not credit.
	if e.Flags & ^uint32(0x1|0x800) != 0 {
		return 0, end, nil
	}
	if e.Length > math.MaxInt64 {
		return 0, start, ErrIdentity
	}
	return int64(e.Length), end, nil
}
