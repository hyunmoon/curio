//go:build linux

package sdrscratch

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/require"
)

func TestKernelRecordShortReads(t *testing.T) {
	for _, wrap := range []func(io.Reader) io.Reader{iotest.OneByteReader, iotest.HalfReader, iotest.DataErrReader} {
		for _, content := range []string{"", "123\n456\n", strings.Repeat("x", 4096), strings.Repeat("x", 4097)} {
			b, err := readKernelRecord(wrap(strings.NewReader(content)))
			if len(content) > 4096 {
				require.ErrorContains(t, err, "oversized kernel record")
				require.Nil(t, b)
			} else {
				require.NoError(t, err)
				require.Equal(t, content, string(b))
			}
		}
	}
}

func TestKernelRecordReadError(t *testing.T) {
	want := errors.New("injected read failure")
	b, err := readKernelRecord(io.MultiReader(strings.NewReader("populated "), iotest.ErrReader(want)))
	require.ErrorIs(t, err, want)
	require.Nil(t, b, "do not return partial evidence as a successful record")
	b, err = readKernelRecord(iotest.ErrReader(io.ErrUnexpectedEOF))
	require.ErrorIs(t, err, io.ErrUnexpectedEOF, "only normal EOF ends a record")
	require.Nil(t, b)
}
