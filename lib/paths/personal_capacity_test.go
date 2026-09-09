package paths

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/filecoin-project/lotus/storage/sealer/fsutil"
)

type personalCapacityStorage struct {
	LocalStorage
	used  int64
	err   error
	paths []string
}

func (s *personalCapacityStorage) Stat(string) (fsutil.FsStat, error) {
	return fsutil.FsStat{Capacity: 1000, Available: 100, FSAvailable: 100, Used: 900}, nil
}

func (s *personalCapacityStorage) DiskUsage(p string) (int64, error) {
	s.paths = append(s.paths, p)
	return s.used, s.err
}

func TestPersonalCapacityProfileValidation(t *testing.T) {
	for _, value := range []string{"disabled", "SDISK-SM", " sdisk-sm", "sdisk-sm ", "sdisk", "/sdisk/sealworker"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv(PERSONAL_STORAGE_PROFILE_ENV, value)
			if _, err := NewLocal(context.Background(), nil, nil, ""); err == nil {
				t.Fatal("malformed profile did not fail before opening storage")
			}
		})
	}
	if p, err := parsePersonalCapacityProfile(""); p != nil || err != nil {
		t.Fatalf("default changed: %v %v", p, err)
	}
}

func TestPersonalCapacityExactPathAndArithmetic(t *testing.T) {
	for _, name := range []string{"sdisk-4slot", "sdisk-sm"} {
		profile, err := parsePersonalCapacityProfile(name)
		if err != nil {
			t.Fatal(err)
		}
		wantCap := int64(5600000000000)
		if name == "sdisk-sm" {
			wantCap = 16800000000000
		}
		for _, local := range []string{profile.root, profile.root + "/child", profile.root + "//child/.."} {
			for _, sample := range []struct{ used, reserved int64 }{{25, 10}, {25, wantCap}, {wantCap, 0}, {wantCap + 1, 2}, {math.MaxInt64, math.MaxInt64}} {
				s := &personalCapacityStorage{used: sample.used}
				p := &path{Local: local, MaxStorage: 1, Reserved: sample.reserved, personalCapacity: profile}
				got, onDisk, err := p.stat(s)
				if err != nil {
					t.Fatal(err)
				}
				avail := max(int64(0), wantCap-sample.used)
				if got.Capacity != wantCap || got.Max != wantCap || got.Used != sample.used || got.FSAvailable != avail || got.Available != max(int64(0), avail-sample.reserved) || got.Reserved != sample.reserved || onDisk != 0 {
					t.Fatalf("profile=%s local=%s used=%d reserved=%d got=%+v", name, local, sample.used, sample.reserved, got)
				}
				if len(s.paths) != 1 || s.paths[0] != local {
					t.Fatalf("usage escaped selected local path: %v", s.paths)
				}
			}
		}
	}
}

func TestPersonalCapacityDisabledAndUnmatched(t *testing.T) {
	for _, name := range []string{"", "sdisk-4slot", "sdisk-sm"} {
		profile, err := parsePersonalCapacityProfile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, local := range []string{"/sdisk", "/sdisk/sealworker-other", "/sdisk_sm/sealworker-other", "/other/sealworker", "sdisk/sealworker"} {
			s := &personalCapacityStorage{used: 25}
			got, _, err := (&path{Local: local, Reserved: 10, personalCapacity: profile}).stat(s)
			if err != nil || got.Capacity != 1000 || got.Available != 90 || got.FSAvailable != 100 || len(s.paths) != 0 {
				t.Fatalf("unmatched/default behavior changed: %+v %v", got, err)
			}
		}
		if profile == nil {
			s := &personalCapacityStorage{used: 25}
			got, _, err := (&path{Local: "/sdisk/sealworker", MaxStorage: 50}).stat(s)
			if err != nil || got.Capacity != 1000 || got.Max != 50 || got.Available != 25 {
				t.Fatalf("legacy MaxStorage changed: %+v %v", got, err)
			}
		}
	}
}

func TestPersonalCapacityUsageFailure(t *testing.T) {
	profile, _ := parsePersonalCapacityProfile("sdisk-4slot")
	for _, s := range []*personalCapacityStorage{{used: -1}, {err: errors.New("synthetic disk failure")}} {
		got, _, err := (&path{Local: profile.root, personalCapacity: profile}).stat(s)
		if err == nil || got.Capacity != 0 || got.Available != 0 {
			t.Fatalf("bad usage fabricated capacity: %+v %v", got, err)
		}
	}
}
