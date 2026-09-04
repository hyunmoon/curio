package sealsupra

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPlanCCAllocationsHonorsQuotaAndRedistributes(t *testing.T) {
	schedules := []ccSchedule{
		{SpID: 101, ToSeal: 100, Weight: 1},
		{SpID: 202, ToSeal: 1, Weight: 1},
	}

	allocations, err := planCCAllocations(schedules, 64)
	require.NoError(t, err)
	require.Len(t, allocations, 2)
	require.Equal(t, int64(63), allocations[0].count)
	require.Equal(t, int64(1), allocations[1].count)

	var total int64
	for _, allocation := range allocations {
		total += allocation.count
		require.LessOrEqual(t, allocation.count, allocation.schedule.ToSeal)
	}
	require.Equal(t, int64(64), total)
}
