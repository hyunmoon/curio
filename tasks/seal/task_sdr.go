package seal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"os"
	"sync"
	"time"

	"go.opencensus.io/stats"
	"go.opencensus.io/tag"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/crypto"

	"github.com/filecoin-project/curio/build"
	"github.com/filecoin-project/curio/harmony/harmonydb"
	"github.com/filecoin-project/curio/harmony/harmonytask"
	"github.com/filecoin-project/curio/harmony/resources"
	"github.com/filecoin-project/curio/harmony/taskhelp"
	"github.com/filecoin-project/curio/lib/dealdata"
	ffi2 "github.com/filecoin-project/curio/lib/ffi"
	"github.com/filecoin-project/curio/lib/paths"
	storiface "github.com/filecoin-project/curio/lib/storiface"
	"github.com/filecoin-project/curio/tasks/tasknames"

	"github.com/filecoin-project/lotus/chain/actors/policy"
	"github.com/filecoin-project/lotus/chain/types"
	"strconv"
	"strings"
)

var IsDevnet = build.BlockDelaySecs < 30

func SetDevnet(value bool) {
	IsDevnet = value
}

func GetDevnet() bool {
	return IsDevnet
}

type SDRAPI interface {
	ChainHead(context.Context) (*types.TipSet, error)
	StateGetRandomnessFromTickets(context.Context, crypto.DomainSeparationTag, abi.ChainEpoch, []byte, types.TipSetKey) (abi.Randomness, error)
}

type SDRTask struct {
	api SDRAPI
	db  *harmonydb.DB
	sp  *SealPoller

	sc *ffi2.SealCalls

	max taskhelp.Limiter
	min int

	minStartInterval time.Duration
	startJitter      bool
	jitterKey        string
	jitterOffset     time.Duration
	jitterWaitUntil  time.Time

	lastSDRStartLk  sync.Mutex
	lastSDRStart    time.Time
	lastSDRDelayLog time.Time
}

func NewSDRTask(api SDRAPI, db *harmonydb.DB, sp *SealPoller, sc *ffi2.SealCalls, maxSDR taskhelp.Limiter, minSDR int, minStartInterval time.Duration, startJitter bool) *SDRTask {
	jitterKey := os.Getenv("CURIO_NODE_NAME")
	if startJitter && jitterKey == "" {
		var err error
		jitterKey, err = os.Hostname()
		if err != nil {
			log.Warnw("getting hostname for SDR start jitter", "error", err)
		}
		log.Warnw("CURIO_NODE_NAME is not set for SDR start jitter; falling back to hostname", "jitter_key", jitterKey)
	}

	var jitterOffset time.Duration
	if startJitter && minStartInterval > 0 && jitterKey != "" {
		sum := sha256.Sum256([]byte(jitterKey))
		jitterOffset = time.Duration(binary.BigEndian.Uint64(sum[:8]) % uint64(minStartInterval.Nanoseconds()))
		log.Infow("SDR start jitter configured", "jitter_key", jitterKey, "interval", minStartInterval, "offset", jitterOffset)
	}

	return &SDRTask{
		api:              api,
		db:               db,
		sp:               sp,
		sc:               sc,
		max:              maxSDR,
		min:              minSDR,
		minStartInterval: minStartInterval,
		startJitter:      startJitter,
		jitterKey:        jitterKey,
		jitterOffset:     jitterOffset,
	}
}

// reserveSDRStartSlot reserves this node's next SDR start slot.
// It must be called from CanAccept, before the harmony task is claimed.
func (s *SDRTask) reserveSDRStartSlot(candidateCount int) bool {
	if s.minStartInterval <= 0 {
		return true
	}

	now := time.Now()
	ready, nextStart, remaining, reason := s.sdrStartReady(now)
	if !ready {
		s.logSDRStartDelayed(reason, nextStart, remaining, candidateCount, 0, false)
		log.Debugw("did not accept task", "name", "SDR", "reason", reason, "interval", s.minStartInterval, "next_start_at", nextStart, "remaining", remaining, "count", candidateCount)
		return false
	}

	s.lastSDRStartLk.Lock()
	defer s.lastSDRStartLk.Unlock()

	now = time.Now()
	ready, nextStart, remaining, reason = s.sdrStartReadyLocked(now)
	if !ready {
		s.logSDRStartDelayed(reason, nextStart, remaining, candidateCount, 0, false)
		log.Debugw("did not accept task", "name", "SDR", "reason", reason, "interval", s.minStartInterval, "next_start_at", nextStart, "remaining", remaining, "count", candidateCount)
		return false
	}

	s.lastSDRStart = now
	s.jitterWaitUntil = time.Time{}

	log.Infow("reserved SDR start slot", "candidate_count", candidateCount, "min_start_interval", s.minStartInterval, "next_start_at", now.Add(s.minStartInterval), "start_jitter", s.startJitter, "jitter_key", s.jitterKey, "jitter_offset", s.jitterOffset)

	return true
}

func (s *SDRTask) sdrStartReady(now time.Time) (bool, time.Time, time.Duration, string) {
	s.lastSDRStartLk.Lock()
	defer s.lastSDRStartLk.Unlock()

	return s.sdrStartReadyLocked(now)
}

func (s *SDRTask) sdrStartReadyLocked(now time.Time) (bool, time.Time, time.Duration, string) {
	if s.minStartInterval <= 0 {
		return true, time.Time{}, 0, ""
	}

	if !s.lastSDRStart.IsZero() {
		nextStart := s.lastSDRStart.Add(s.minStartInterval)
		if now.Before(nextStart) {
			return false, nextStart, nextStart.Sub(now), "min start interval"
		}
	}

	if s.startJitter && s.jitterKey != "" {
		idleThreshold := 2 * s.minStartInterval
		if s.jitterWaitUntil.IsZero() && (s.lastSDRStart.IsZero() || now.Sub(s.lastSDRStart) > idleThreshold) {
			s.jitterWaitUntil = nextSDRJitterPhase(now, s.minStartInterval, s.jitterOffset)
			log.Infow("SDR start jitter waiting for phase", "jitter_key", s.jitterKey, "interval", s.minStartInterval, "offset", s.jitterOffset, "next_start_at", s.jitterWaitUntil, "remaining", time.Until(s.jitterWaitUntil))
		}

		if !s.jitterWaitUntil.IsZero() && now.Before(s.jitterWaitUntil) {
			return false, s.jitterWaitUntil, s.jitterWaitUntil.Sub(now), "start jitter phase"
		}
	}

	return true, time.Time{}, 0, ""
}

func (s *SDRTask) logSDRStartDelayed(reason string, nextStart time.Time, remaining time.Duration, candidateCount int, taskID harmonytask.TaskID, includeTask bool) {
	if s.minStartInterval <= 0 {
		return
	}

	now := time.Now()

	s.lastSDRStartLk.Lock()
	if !s.lastSDRDelayLog.IsZero() && now.Sub(s.lastSDRDelayLog) < time.Minute {
		s.lastSDRStartLk.Unlock()
		return
	}

	s.lastSDRDelayLog = now
	lastStart := s.lastSDRStart
	jitterWaitUntil := s.jitterWaitUntil
	s.lastSDRStartLk.Unlock()

	fields := []interface{}{
		"reason", reason,
		"min_start_interval", s.minStartInterval,
		"next_start_at", nextStart,
		"remaining", remaining,
		"candidate_count", candidateCount,
		"last_sdr_start", lastStart,
		"start_jitter", s.startJitter,
		"jitter_key", s.jitterKey,
		"jitter_offset", s.jitterOffset,
		"jitter_wait_until", jitterWaitUntil,
	}

	if includeTask {
		fields = append(fields, "task", taskID)
	}

	log.Infow("SDR start delayed", fields...)
}

func nextSDRJitterPhase(now time.Time, interval time.Duration, offset time.Duration) time.Time {
	if interval <= 0 {
		return now
	}

	intervalN := int64(interval)
	offsetN := int64(offset)
	nowN := now.UnixNano()

	rem := (nowN - offsetN) % intervalN
	if rem < 0 {
		rem += intervalN
	}

	wait := intervalN - rem
	if wait == intervalN {
		wait = 0
	}

	return now.Add(time.Duration(wait))
}

func (s *SDRTask) Do(taskID harmonytask.TaskID, stillOwned func() bool) (done bool, err error) {
	ctx := context.Background()

	var sectorParamsArr []struct {
		SpID         int64                   `db:"sp_id"`
		SectorNumber int64                   `db:"sector_number"`
		RegSealProof abi.RegisteredSealProof `db:"reg_seal_proof"`
	}

	err = s.db.Select(ctx, &sectorParamsArr, `
		SELECT sp_id, sector_number, reg_seal_proof
		FROM sectors_sdr_pipeline
		WHERE task_id_sdr = $1`, taskID)
	if err != nil {
		return false, xerrors.Errorf("getting sector params: %w", err)
	}

	if len(sectorParamsArr) != 1 {
		return false, xerrors.Errorf("expected 1 sector params, got %d", len(sectorParamsArr))
	}
	sectorParams := sectorParamsArr[0]

	dealData, err := dealdata.DealDataSDRPoRep(ctx, s.db, s.sc, sectorParams.SpID, sectorParams.SectorNumber, sectorParams.RegSealProof, true)
	if err != nil {
		return false, xerrors.Errorf("getting deal data: %w", err)
	}

	sref := storiface.SectorRef{
		ID: abi.SectorID{
			Miner:  abi.ActorID(sectorParams.SpID),
			Number: abi.SectorNumber(sectorParams.SectorNumber),
		},
		ProofType: sectorParams.RegSealProof,
	}

	// get ticket
	maddr, err := address.NewIDAddress(uint64(sectorParams.SpID))
	if err != nil {
		return false, xerrors.Errorf("getting miner address: %w", err)
	}

	// FAIL: api may be down
	// FAIL-RESP: rely on harmony retry
	ticket, ticketEpoch, err := GetTicket(ctx, s.api, maddr)
	if err != nil {
		return false, xerrors.Errorf("getting ticket: %w", err)
	}

	// do the SDR!!

	// FAIL: storage may not have enough space
	// FAIL-RESP: rely on harmony retry

	// LATEFAIL: compute error in sdr
	// LATEFAIL-RESP: Check in Trees task should catch this; Will retry computing
	//                Trees; After one retry, it should return the sector to the
	// 			      SDR stage; max number of retries should be configurable

	err = s.sc.GenerateSDR(ctx, taskID, storiface.FTCache, sref, ticket, dealData.CommD)
	if err != nil {
		return false, xerrors.Errorf("generating sdr: %w", err)
	}

	// store success!
	n, err := s.db.Exec(ctx, `UPDATE sectors_sdr_pipeline
		SET after_sdr = true, ticket_epoch = $3, ticket_value = $4, task_id_sdr = NULL
		WHERE sp_id = $1 AND sector_number = $2`,
		sectorParams.SpID, sectorParams.SectorNumber, ticketEpoch, []byte(ticket))
	if err != nil {
		return false, xerrors.Errorf("store sdr success: updating pipeline: %w", err)
	}
	if n != 1 {
		return false, xerrors.Errorf("store sdr success: updated %d rows", n)
	}

	// Record metric
	if err := stats.RecordWithTags(ctx, []tag.Mutator{
		tag.Upsert(MinerTag, maddr.String()),
	}, SealMeasures.SDRCompleted.M(1)); err != nil {
		log.Errorf("recording metric: %s", err)
	}

	return true, nil
}

type TicketNodeAPI interface {
	ChainHead(context.Context) (*types.TipSet, error)
	StateGetRandomnessFromTickets(context.Context, crypto.DomainSeparationTag, abi.ChainEpoch, []byte, types.TipSetKey) (abi.Randomness, error)
}

func GetTicket(ctx context.Context, api TicketNodeAPI, maddr address.Address) (abi.SealRandomness, abi.ChainEpoch, error) {
	ts, err := api.ChainHead(ctx)
	if err != nil {
		return nil, 0, xerrors.Errorf("getting chain head: %w", err)
	}

	ticketEpoch := ts.Height() - policy.SealRandomnessLookback
	buf := new(bytes.Buffer)
	if err := maddr.MarshalCBOR(buf); err != nil {
		return nil, 0, xerrors.Errorf("marshaling miner address: %w", err)
	}

	rand, err := api.StateGetRandomnessFromTickets(ctx, crypto.DomainSeparationTag_SealRandomness, ticketEpoch, buf.Bytes(), ts.Key())
	if err != nil {
		return nil, 0, xerrors.Errorf("getting randomness from tickets: %w", err)
	}

	return abi.SealRandomness(rand), ticketEpoch, nil
}

func (s *SDRTask) CanAccept(ids []harmonytask.TaskID, _ *harmonytask.TaskEngine) ([]harmonytask.TaskID, error) {
	if s.min > len(ids) {
		log.Debugw("did not accept task", "name", "SDR", "reason", "below min", "min", s.min, "count", len(ids))
		return []harmonytask.TaskID{}, nil
	}

	if len(ids) == 0 || s.minStartInterval <= 0 {
		return ids, nil
	}

	if !s.reserveSDRStartSlot(len(ids)) {
		return []harmonytask.TaskID{}, nil
	}

	if len(ids) > 1 {
		return ids[:1], nil
	}

	return ids, nil
}

func sdrCPUCostFromEnv() int {
	const defaultSDRCPUCost = 4

	raw := strings.TrimSpace(os.Getenv("FIL_PROOFS_MULTICORE_SDR_PRODUCERS"))
	if raw == "" {
		return defaultSDRCPUCost
	}

	producers, err := strconv.Atoi(raw)
	if err != nil {
		log.Warnw("invalid FIL_PROOFS_MULTICORE_SDR_PRODUCERS, using default SDR CPU cost", "value", raw, "error", err, "default_cpu_cost", defaultSDRCPUCost)
		return defaultSDRCPUCost
	}

	if producers < 1 || producers > 3 {
		log.Warnw("FIL_PROOFS_MULTICORE_SDR_PRODUCERS out of range, using default SDR CPU cost", "value", raw, "default_cpu_cost", defaultSDRCPUCost)
		return defaultSDRCPUCost
	}

	// filecoin-ffi supports producer values 1..3, corresponding roughly to 2..4 CPU cores.
	return producers + 1
}

func (s *SDRTask) TypeDetails() harmonytask.TaskTypeDetails {
	ssize := abi.SectorSize(32 << 30) // todo task details needs taskID to get correct sector size
	if IsDevnet {
		ssize = abi.SectorSize(2 << 20)
	}

	res := harmonytask.TaskTypeDetails{
		Max:  s.max,
		Name: tasknames.SDR,
		Cost: resources.Resources{
			Cpu:     sdrCPUCostFromEnv(), // based on FIL_PROOFS_MULTICORE_SDR_PRODUCERS
			Gpu:     0,
			Ram:     (64 << 30) + (256 << 20),
			Storage: s.sc.Storage(s.taskToSector, storiface.FTCache, storiface.FTNone, ssize, storiface.PathSealing, paths.MinFreeStoragePercentage),
		},
		MaxFailures: 2,
		Follows:     nil,
	}

	if IsDevnet {
		res.Cost.Ram = 1 << 30
		res.Cost.Cpu = 1
	}

	return res
}

func (s *SDRTask) Adder(taskFunc harmonytask.AddTaskFunc) {
	s.sp.pollers[pollerSDR].Set(taskFunc)
}

func (s *SDRTask) GetSpid(db *harmonydb.DB, taskID int64) string {
	sid, err := s.GetSectorID(db, taskID)
	if err != nil {
		log.Errorf("getting sector id: %s", err)
		return ""
	}
	return sid.Miner.String()
}

func (s *SDRTask) GetSectorID(db *harmonydb.DB, taskID int64) (*abi.SectorID, error) {
	var spId, sectorNumber uint64
	err := db.QueryRow(context.Background(), `SELECT sp_id,sector_number FROM sectors_sdr_pipeline WHERE task_id_sdr = $1`, taskID).Scan(&spId, &sectorNumber)
	if err != nil {
		return nil, err
	}
	return &abi.SectorID{
		Miner:  abi.ActorID(spId),
		Number: abi.SectorNumber(sectorNumber),
	}, nil
}

var _ = harmonytask.Reg(&SDRTask{})

func (s *SDRTask) taskToSector(id harmonytask.TaskID) (ffi2.SectorRef, error) {
	var refs []ffi2.SectorRef

	err := s.db.Select(context.Background(), &refs, `SELECT sp_id, sector_number, reg_seal_proof FROM sectors_sdr_pipeline WHERE task_id_sdr = $1`, id)
	if err != nil {
		return ffi2.SectorRef{}, xerrors.Errorf("getting sector ref: %w", err)
	}

	if len(refs) != 1 {
		return ffi2.SectorRef{}, xerrors.Errorf("expected 1 sector ref, got %d", len(refs))
	}

	return refs[0], nil
}

var _ harmonytask.TaskInterface = &SDRTask{}
