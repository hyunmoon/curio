package storage_market

import (
	"testing"

	"github.com/stretchr/testify/require"
)

type mk20OffsetContentionVerdict string

const (
	mk20OffsetContentionObservedBlocking   mk20OffsetContentionVerdict = "observed-blocking"
	mk20OffsetContentionRecognizedConflict mk20OffsetContentionVerdict = "recognized-conflict"
	mk20OffsetContentionInconclusive       mk20OffsetContentionVerdict = "inconclusive"
	mk20OffsetContentionOrderingViolation  mk20OffsetContentionVerdict = "ordering-violation"
)

type mk20OffsetContentionEvidence struct {
	WaitObserved                      bool
	ObservationFailed                 bool
	RecognizedConflict                bool
	StatementReturnedBeforeRelease    bool
	StatementSucceededBeforeRelease   bool
	TransactionFinished               bool
	TransactionCommitted              bool
	TransactionFinishedBeforeRelease  bool
	TransactionCommittedBeforeRelease bool
}

// classifyMK20OffsetContention classifies contention evidence only. The
// tagged SQL tests verify final row state and rollback before accepting this
// verdict.
func classifyMK20OffsetContention(evidence mk20OffsetContentionEvidence) mk20OffsetContentionVerdict {
	if evidence.TransactionFinishedBeforeRelease && evidence.TransactionCommittedBeforeRelease {
		return mk20OffsetContentionOrderingViolation
	}
	if evidence.WaitObserved {
		return mk20OffsetContentionObservedBlocking
	}
	if evidence.RecognizedConflict {
		return mk20OffsetContentionRecognizedConflict
	}
	return mk20OffsetContentionInconclusive
}

func TestClassifyMK20OffsetContentionEvidence(t *testing.T) {
	tests := []struct {
		name     string
		evidence mk20OffsetContentionEvidence
		expected mk20OffsetContentionVerdict
	}{
		{
			name:     "observed database blocking",
			evidence: mk20OffsetContentionEvidence{WaitObserved: true},
			expected: mk20OffsetContentionObservedBlocking,
		},
		{
			name:     "recognized serialization conflict",
			evidence: mk20OffsetContentionEvidence{RecognizedConflict: true},
			expected: mk20OffsetContentionRecognizedConflict,
		},
		{
			name: "recognized conflict remains evidence when lock observation fails",
			evidence: mk20OffsetContentionEvidence{
				ObservationFailed:  true,
				RecognizedConflict: true,
			},
			expected: mk20OffsetContentionRecognizedConflict,
		},
		{
			name: "Yugabyte successful final values without wait or conflict are inconclusive",
			evidence: mk20OffsetContentionEvidence{
				StatementReturnedBeforeRelease: true,
				TransactionFinished:            true,
				TransactionCommitted:           true,
			},
			expected: mk20OffsetContentionInconclusive,
		},
		{
			name: "successful statement is not a successful transaction",
			evidence: mk20OffsetContentionEvidence{
				StatementReturnedBeforeRelease:  true,
				StatementSucceededBeforeRelease: true,
			},
			expected: mk20OffsetContentionInconclusive,
		},
		{
			name: "finished transaction without a successful commit is not ordering evidence",
			evidence: mk20OffsetContentionEvidence{
				TransactionFinishedBeforeRelease: true,
			},
			expected: mk20OffsetContentionInconclusive,
		},
		{
			name: "observation failure without independent evidence is inconclusive",
			evidence: mk20OffsetContentionEvidence{
				ObservationFailed: true,
			},
			expected: mk20OffsetContentionInconclusive,
		},
		{
			name: "successful transaction before resolver release violates ordering",
			evidence: mk20OffsetContentionEvidence{
				WaitObserved:                      true,
				RecognizedConflict:                true,
				TransactionFinishedBeforeRelease:  true,
				TransactionCommittedBeforeRelease: true,
			},
			expected: mk20OffsetContentionOrderingViolation,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.expected, classifyMK20OffsetContention(test.evidence))
		})
	}
}
