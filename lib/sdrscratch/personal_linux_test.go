//go:build linux

package sdrscratch

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestPersonalRequiresDelegationNotRootAssumption(t *testing.T) {
	p := map[string]string{"ControlGroup": "/system.slice/example.service", "Type": "simple", "Delegate": "no", "KillMode": "control-group", "SendSIGKILL": "yes"}
	require.ErrorContains(t, personalService(p["ControlGroup"], p), "Delegate=yes")
	p["Delegate"] = "yes"
	require.NoError(t, personalService(p["ControlGroup"], p))
	p["Type"] = "notify"
	require.Error(t, personalService(p["ControlGroup"], p))
}

// Same test executable acts as the replacement Curio/native body. Only the
// persistent state path is relocated to this owned fixture. No helper binary.
func TestPersonalLinuxWorker(t *testing.T) {
	mode := os.Getenv("CURIO_SDR_PERSONAL_TEST_MODE")
	if mode == "" {
		return
	}
	require.Equal(t, "disposable", os.Getenv("CURIO_SDR_DISCARD_TEST_TARGET"))
	control := os.Getenv("CURIO_SDR_PERSONAL_TEST_CONTROL")
	var roots []string
	require.NoError(t, json.Unmarshal([]byte(os.Getenv("CURIO_SDR_PERSONAL_TEST_ROOTS")), &roots))
	args, env, pid := append([]string(nil), os.Args...), append([]string(nil), os.Environ()...), os.Getpid()
	cwd, err := os.Getwd()
	require.NoError(t, err)
	on, err := PersonalCleanupEnabled()
	require.NoError(t, err)
	require.True(t, on)
	s, err := startPersonalAt(roots, filepath.Join(control, "state"))
	if mode == "reject" {
		require.ErrorContains(t, err, "Delegate=yes")
		require.NoError(t, os.WriteFile(filepath.Join(control, "rejected"), []byte("PASS"), 0600))
		return
	}
	require.NoError(t, err)
	personalSessionPtr.Store(s)
	for i, root := range roots {
		require.NoError(t, RegisterPersonalStorage(root, fmt.Sprint(i)))
		base := filepath.Join(root, "cache")
		p := filepath.Join(base, "s-t01000-42.sdr.tmp", Prefix+uuid.NewString())
		w, err := Begin(p)
		require.NoError(t, err)
		require.True(t, w.DiscardInterrupted())
		require.NoError(t, os.WriteFile(filepath.Join(p, "layer"), []byte("replacement native output"), 0600))
		require.NoError(t, w.Returned())
		n, err := w.DiscardOwn()
		require.NoError(t, err)
		require.Equal(t, 1, n)
		require.NoError(t, w.Close())
		require.FileExists(t, filepath.Join(base, "s-t01000-42", "keep"))
	}
	now, err := os.Getwd()
	require.NoError(t, err)
	require.Equal(t, cwd, now)
	require.Equal(t, pid, os.Getpid())
	require.True(t, reflect.DeepEqual(args, os.Args))
	require.True(t, reflect.DeepEqual(env, os.Environ()))
	require.NoError(t, os.WriteFile(filepath.Join(control, "passed"), []byte("PASS"), 0600))
}

func TestPersonalLinuxReviewSameBinary(t *testing.T) {
	root := reviewLinux(t)
	control, err := os.MkdirTemp(root, "single-binary-")
	require.NoError(t, err)
	var roots []string
	for i := 0; i < 2; i++ {
		mount := filepath.Join(control, fmt.Sprintf("disk-%d", i))
		image := mount + ".img"
		require.NoError(t, os.Mkdir(mount, 0700))
		f, err := os.OpenFile(image, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		require.NoError(t, err)
		require.NoError(t, f.Truncate(64<<20))
		require.NoError(t, f.Close())
		reviewCommand(t, "mkfs.ext4", "-q", "-F", image)
		reviewCommand(t, "mount", "-o", "loop", image, mount)
		t.Cleanup(func() { require.NoError(t, unix.Unmount(mount, 0)) })
		storage := filepath.Join(mount, "registered-root")
		require.NoError(t, os.Mkdir(storage, 0700))
		require.NoError(t, os.Mkdir(filepath.Join(storage, "cache"), 0700))
		require.NoError(t, os.Mkdir(filepath.Join(storage, "cache", "s-t01000-42"), 0700))
		require.NoError(t, os.WriteFile(filepath.Join(storage, "cache", "s-t01000-42", "keep"), []byte("canonical"), 0600))
		b, err := json.Marshal(map[string]any{"ID": fmt.Sprint(i), "CanSeal": true})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(storage, "sectorstore.json"), b, 0600))
		roots = append(roots, storage)
	}
	raw, err := json.Marshal(roots)
	require.NoError(t, err)
	for _, mode := range []string{"reject", "run"} {
		name := "curio-personal-fixture-" + uuid.NewString() + ".service"
		unit := filepath.Join("/usr/local/lib/systemd/system", name)
		require.NoError(t, os.MkdirAll(filepath.Dir(unit), 0755))
		_, err := os.Lstat(unit)
		require.True(t, os.IsNotExist(err))
		delegate := "yes"
		if mode == "reject" {
			delegate = "no"
		}
		text := fmt.Sprintf("[Service]\nType=simple\nDelegate=%s\nKillMode=control-group\nSendSIGKILL=yes\nExecStart=%s -test.run=^TestPersonalLinuxWorker$ -test.timeout=40s\nEnvironment=CURIO_PERSONAL_SDR_CLEANUP=1\nEnvironment=CURIO_SDR_DISCARD_TEST_TARGET=disposable\nEnvironment=CURIO_SDR_PERSONAL_TEST_MODE=%s\nEnvironment=CURIO_SDR_PERSONAL_TEST_CONTROL=%s\nEnvironment='CURIO_SDR_PERSONAL_TEST_ROOTS=%s'\n", delegate, os.Args[0], mode, control, string(raw))
		require.False(t, strings.ContainsAny(os.Args[0], " %\n"))
		require.NoError(t, os.WriteFile(unit, []byte(text), 0644))
		t.Cleanup(func() {
			reviewCommand(t, "systemctl", "stop", name)
			require.NoError(t, os.Remove(unit))
			reviewCommand(t, "systemctl", "daemon-reload")
		})
		reviewCommand(t, "systemctl", "daemon-reload")
		reviewCommand(t, "systemctl", "start", name)
		marker := "passed"
		if mode == "reject" {
			marker = "rejected"
		}
		require.Eventually(t, func() bool { _, e := os.Stat(filepath.Join(control, marker)); return e == nil }, 20*time.Second, 20*time.Millisecond)
		reviewCommand(t, "systemctl", "stop", name)
	}
}
