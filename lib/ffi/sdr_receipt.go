package ffi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ipfs/go-cid"
	"golang.org/x/sys/unix"

	commcid "github.com/filecoin-project/go-fil-commcid"
	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/harmony/harmonytask"
	"github.com/filecoin-project/curio/lib/paths"
	"github.com/filecoin-project/curio/lib/proofpaths"
	"github.com/filecoin-project/curio/lib/storiface"
)

// Attached before publication to the output inode itself: unlike a sidecar,
// this cannot become detached from an FTKey file across an atomic rename.
// Filesystems without user xattrs fail before native work. This is provenance
// for retrying our own successful output, not proof of native liveness or a
// checksum/authentication scheme for untrusted files.
const sdrReceiptAttribute = "user.curio.sdr-completion-v1"

type sdrReservation = paths.SDRReservation

func prepareSDRReservation(dest string, s storiface.SectorRef, ft storiface.SectorFileType) (*paths.SDRReservation, error) {
	reservation := paths.NewSDRReservation(dest)
	r, err := readSDRReceipt(dest)
	if err != nil {
		return nil, err
	}
	if r != nil {
		if r.Sector != s.ID || r.FileType != ft || r.Proof != s.ProofType {
			return nil, fmt.Errorf("SDR reservation output identity mismatch")
		}
		// Do must either validate and reuse this exact output or reject before
		// generating anything. It cannot spend this credit on fresh computation.
		reservation.UsePublished(dest)
	}
	return reservation, nil
}

type SDRSealTicket struct {
	Epoch abi.ChainEpoch
	Value abi.SealRandomness
}

type sdrReceipt struct {
	Version     int
	Sector      abi.SectorID
	FileType    storiface.SectorFileType
	Proof       abi.RegisteredSealProof
	CommD       string
	Ticket      abi.SealRandomness
	TicketEpoch *abi.ChainEpoch
	ReplicaID   [32]byte
	Layers      []int64
}

func newSDRReceipt(ft storiface.SectorFileType, s storiface.SectorRef, ticket abi.SealRandomness, d cid.Cid, epoch []abi.ChainEpoch) (sdrReceipt, error) {
	r := sdrReceipt{Version: 1, Sector: s.ID, FileType: ft, Proof: s.ProofType, CommD: d.String(), Ticket: ticket}
	if len(ticket) != 32 || len(epoch) > 1 {
		return r, fmt.Errorf("invalid SDR ticket or epoch")
	}
	if len(epoch) == 1 {
		r.TicketEpoch = &epoch[0]
	}
	commD, err := commcid.CIDToDataCommitmentV1(d)
	if err != nil {
		return r, err
	}
	r.ReplicaID, err = s.ProofType.ReplicaId(s.ID.Miner, s.ID.Number, ticket, commD)
	return r, err
}

func sdrLayerSizes(dest string, r sdrReceipt) ([]int64, error) {
	n, err := proofpaths.SDRLayers(r.Proof)
	if err != nil {
		return nil, err
	}
	var names []string
	switch r.FileType {
	case storiface.FTCache:
		st, err := os.Lstat(dest)
		if err != nil {
			return nil, err
		}
		if !st.IsDir() {
			return nil, fmt.Errorf("SDR cache is not a directory")
		}
		for i := 1; i <= n; i++ {
			names = append(names, filepath.Join(dest, proofpaths.LayerFileName(i)))
		}
	case storiface.FTKey:
		names = []string{dest}
	default:
		return nil, fmt.Errorf("invalid SDR receipt file type")
	}
	sizes := make([]int64, len(names))
	for i, p := range names {
		st, err := os.Lstat(p)
		if err != nil {
			return nil, err
		}
		if !st.Mode().IsRegular() || st.Size() <= 0 {
			return nil, fmt.Errorf("invalid SDR layer %s", p)
		}
		sizes[i] = st.Size()
	}
	return sizes, nil
}

func readSDRReceipt(dest string) (*sdrReceipt, error) {
	st, err := os.Lstat(dest)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("SDR destination is a symlink")
	}
	b := make([]byte, 16384)
	n, err := unix.Getxattr(dest, sdrReceiptAttribute, b)
	if err != nil {
		return nil, fmt.Errorf("existing SDR destination has no readable completion receipt: %w", err)
	}
	var r sdrReceipt
	if err = json.Unmarshal(b[:n], &r); err != nil {
		return nil, fmt.Errorf("invalid SDR receipt: %w", err)
	}
	if r.Version != 1 || len(r.Ticket) != 32 {
		return nil, fmt.Errorf("unsupported SDR receipt")
	}
	d, err := cid.Parse(r.CommD)
	if err != nil {
		return nil, err
	}
	expected, err := newSDRReceipt(r.FileType, storiface.SectorRef{ID: r.Sector, ProofType: r.Proof}, r.Ticket, d, nil)
	if err != nil || expected.ReplicaID != r.ReplicaID {
		return nil, fmt.Errorf("invalid SDR replica identity")
	}
	sizes, err := sdrLayerSizes(dest, r)
	if err != nil {
		return nil, err
	}
	if len(sizes) != len(r.Layers) {
		return nil, fmt.Errorf("SDR output layout changed")
	}
	for i, n := range sizes {
		if n != r.Layers[i] {
			return nil, fmt.Errorf("SDR output layer size changed")
		}
	}
	return &r, nil
}

func (r sdrReceipt) matches(want sdrReceipt) bool {
	return r.Sector == want.Sector && r.FileType == want.FileType && r.Proof == want.Proof && r.CommD == want.CommD && bytes.Equal(r.Ticket, want.Ticket) && r.ReplicaID == want.ReplicaID && ((r.TicketEpoch == nil && want.TicketEpoch == nil) || (r.TicketEpoch != nil && want.TicketEpoch != nil && *r.TicketEpoch == *want.TicketEpoch))
}

func writeSDRReceipt(dest string, r sdrReceipt) error {
	var err error
	r.Layers, err = sdrLayerSizes(dest, r)
	if err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err = unix.Setxattr(dest, sdrReceiptAttribute, b, 0); err != nil {
		return err
	}
	f, err := os.Open(dest)
	if err != nil {
		return err
	}
	syncErr := f.Sync()
	return errors.Join(syncErr, f.Close())
}

// SDRTicket returns the original ticket only for a validated, locally reserved
// published cache. The caller must check chain randomness/freshness before use.
func (sb *SealCalls) SDRTicket(taskID harmonytask.TaskID, s storiface.SectorRef, d cid.Cid) (*SDRSealTicket, error) {
	reservations, ok := sb.Sectors.storageReservations.Load(taskID)
	if !ok {
		return nil, fmt.Errorf("SDR has no storage reservation")
	}
	for _, res := range reservations {
		if res.SectorRef.ID() != s.ID {
			continue
		}
		r, err := readSDRReceipt(res.Paths.Cache)
		if err != nil || r == nil {
			return nil, err
		}
		if r.Sector != s.ID || r.FileType != storiface.FTCache || r.Proof != s.ProofType || r.CommD != d.String() || r.TicketEpoch == nil {
			return nil, fmt.Errorf("published SDR inputs do not match this sealing request")
		}
		return &SDRSealTicket{Epoch: *r.TicketEpoch, Value: r.Ticket}, nil
	}
	return nil, fmt.Errorf("SDR has no matching storage reservation")
}
