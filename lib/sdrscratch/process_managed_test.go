package sdrscratch

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestManagedProcessHelper(t *testing.T) {
	p := os.Getenv("SDR_MANAGED_PROCESS_TEST_PATH")
	if p == "" {
		return
	}
	w, err := BeginWithOptions(p, Options{Boundary: new(testBoundary)})
	require.NoError(t, err)
	defer func() { _ = w.Close() }()
	require.NoError(t, os.WriteFile(filepath.Join(p, "layer"), make([]byte, 8192), 0600))
	fmt.Println("ready")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

func TestManagedActualProcessKill(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	p := filepath.Join(base, "s-t01000-42.sdr.tmp", Prefix+uuid.NewString())
	b := new(testBoundary)
	cmd := exec.Command(os.Args[0], "-test.run=^TestManagedProcessHelper$", "-test.timeout=10s")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "SDR_MANAGED_PROCESS_TEST_PATH=" + p}
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	defer func() { _ = stdin.Close() }()
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	s, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "ready\n", s)
	r, err := SweepWithBoundary(base, true, b)
	require.NoError(t, err)
	require.Equal(t, "live", r[0].Status)
	require.NoError(t, cmd.Process.Kill())
	require.Error(t, cmd.Wait())
	// Test supervisor tracks Wait on its actual child. cgroup subtree evidence
	// is NOT exercised on Darwin; the opt-in Linux test has a surviving child.
	b.ended.Store(true)
	r, err = SweepWithBoundary(base, true, b)
	require.NoError(t, err)
	require.Equal(t, 1, r[0].FilesRemoved)
}
