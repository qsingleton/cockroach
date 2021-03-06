// Copyright 2016 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Tobias Schottdorf (tobias.schottdorf@gmail.com)

package storage

import (
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/storage/engine"
	"github.com/cockroachdb/cockroach/pkg/storage/engine/enginepb"
	"github.com/cockroachdb/cockroach/pkg/storage/storagebase"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/coreos/etcd/raft/raftpb"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
)

// replicaStateLoader contains the precomputed range-local replicated and
// unreplicated prefixes. These prefixes are used to avoid an allocation on
// every call to the commonly used range keys (e.g. RaftAppliedIndexKey).
//
// Note that this struct is not safe for concurrent use. It is usually
// protected by Replica.raftMu or a goroutine local variable is used.
type replicaStateLoader struct {
	keys.RangeIDPrefixBuf
}

func makeReplicaStateLoader(rangeID roachpb.RangeID) replicaStateLoader {
	return replicaStateLoader{
		RangeIDPrefixBuf: keys.MakeRangeIDPrefixBuf(rangeID),
	}
}

// loadState loads a ReplicaState from disk. The exception is the Desc field,
// which is updated transactionally, and is populated from the supplied
// RangeDescriptor under the convention that that is the latest committed
// version.
func (rsl replicaStateLoader) load(
	ctx context.Context, reader engine.Reader, desc *roachpb.RangeDescriptor,
) (storagebase.ReplicaState, error) {
	var s storagebase.ReplicaState
	// TODO(tschottdorf): figure out whether this is always synchronous with
	// on-disk state (likely iffy during Split/ChangeReplica triggers).
	s.Desc = protoutil.Clone(desc).(*roachpb.RangeDescriptor)
	// Read the range lease.
	lease, err := rsl.loadLease(ctx, reader)
	if err != nil {
		return storagebase.ReplicaState{}, err
	}
	s.Lease = &lease

	if s.Frozen, err = rsl.loadFrozenStatus(ctx, reader); err != nil {
		return storagebase.ReplicaState{}, err
	}

	if s.GCThreshold, err = rsl.loadGCThreshold(ctx, reader); err != nil {
		return storagebase.ReplicaState{}, err
	}

	if s.TxnSpanGCThreshold, err = rsl.loadTxnSpanGCThreshold(ctx, reader); err != nil {
		return storagebase.ReplicaState{}, err
	}

	if s.RaftAppliedIndex, s.LeaseAppliedIndex, err = rsl.loadAppliedIndex(ctx, reader); err != nil {
		return storagebase.ReplicaState{}, err
	}

	if s.Stats, err = rsl.loadMVCCStats(ctx, reader); err != nil {
		return storagebase.ReplicaState{}, err
	}

	// The truncated state should not be optional (i.e. the pointer is
	// pointless), but it is and the migration is not worth it.
	truncState, err := rsl.loadTruncatedState(ctx, reader)
	if err != nil {
		return storagebase.ReplicaState{}, err
	}
	s.TruncatedState = &truncState

	return s, nil
}

// save persists the given ReplicaState to disk. It assumes that the contained
// Stats are up-to-date and returns the stats which result from writing the
// updated State.
//
// As an exception to the rule, the Desc field (whose on-disk state is special
// in that it's a full MVCC value and updated transactionally) is only used for
// its RangeID.
//
// TODO(tschottdorf): test and assert that none of the optional values are
// missing when- ever saveState is called. Optional values should be reserved
// strictly for use in EvalResult. Do before merge.
func (rsl replicaStateLoader) save(
	ctx context.Context, eng engine.ReadWriter, state storagebase.ReplicaState,
) (enginepb.MVCCStats, error) {
	ms := &state.Stats
	if err := rsl.setLease(ctx, eng, ms, state.Lease); err != nil {
		return enginepb.MVCCStats{}, err
	}
	if err := rsl.setAppliedIndex(
		ctx, eng, ms, state.RaftAppliedIndex, state.LeaseAppliedIndex,
	); err != nil {
		return enginepb.MVCCStats{}, err
	}
	if err := rsl.setFrozenStatus(ctx, eng, ms, state.Frozen); err != nil {
		return enginepb.MVCCStats{}, err
	}
	if err := rsl.setGCThreshold(ctx, eng, ms, &state.GCThreshold); err != nil {
		return enginepb.MVCCStats{}, err
	}
	if err := rsl.setTxnSpanGCThreshold(ctx, eng, ms, &state.TxnSpanGCThreshold); err != nil {
		return enginepb.MVCCStats{}, err
	}
	if err := rsl.setTruncatedState(ctx, eng, ms, state.TruncatedState); err != nil {
		return enginepb.MVCCStats{}, err
	}
	if err := rsl.setMVCCStats(ctx, eng, &state.Stats); err != nil {
		return enginepb.MVCCStats{}, err
	}
	return state.Stats, nil
}

func (rsl replicaStateLoader) loadLease(
	ctx context.Context, reader engine.Reader,
) (roachpb.Lease, error) {
	var lease roachpb.Lease
	_, err := engine.MVCCGetProto(ctx, reader, rsl.RangeLeaseKey(),
		hlc.Timestamp{}, true, nil, &lease)
	return lease, err
}

func (rsl replicaStateLoader) setLease(
	ctx context.Context, eng engine.ReadWriter, ms *enginepb.MVCCStats, lease *roachpb.Lease,
) error {
	if lease == nil {
		return errors.New("cannot persist nil Lease")
	}
	return engine.MVCCPutProto(ctx, eng, ms, rsl.RangeLeaseKey(),
		hlc.Timestamp{}, nil, lease)
}

func loadAppliedIndex(
	ctx context.Context, reader engine.Reader, rangeID roachpb.RangeID,
) (uint64, uint64, error) {
	rsl := makeReplicaStateLoader(rangeID)
	return rsl.loadAppliedIndex(ctx, reader)
}

// loadAppliedIndex returns the Raft applied index and the lease applied index.
func (rsl replicaStateLoader) loadAppliedIndex(
	ctx context.Context, reader engine.Reader,
) (uint64, uint64, error) {
	var appliedIndex uint64
	v, _, err := engine.MVCCGet(ctx, reader, rsl.RaftAppliedIndexKey(),
		hlc.Timestamp{}, true, nil)
	if err != nil {
		return 0, 0, err
	}
	if v != nil {
		int64AppliedIndex, err := v.GetInt()
		if err != nil {
			return 0, 0, err
		}
		appliedIndex = uint64(int64AppliedIndex)
	}
	// TODO(tschottdorf): code duplication.
	var leaseAppliedIndex uint64
	v, _, err = engine.MVCCGet(ctx, reader, rsl.LeaseAppliedIndexKey(),
		hlc.Timestamp{}, true, nil)
	if err != nil {
		return 0, 0, err
	}
	if v != nil {
		int64LeaseAppliedIndex, err := v.GetInt()
		if err != nil {
			return 0, 0, err
		}
		leaseAppliedIndex = uint64(int64LeaseAppliedIndex)
	}

	return appliedIndex, leaseAppliedIndex, nil
}

// setAppliedIndex sets the {raft,lease} applied index values, properly
// accounting for existing keys in the returned stats.
func (rsl replicaStateLoader) setAppliedIndex(
	ctx context.Context,
	eng engine.ReadWriter,
	ms *enginepb.MVCCStats,
	appliedIndex,
	leaseAppliedIndex uint64,
) error {
	var value roachpb.Value
	value.SetInt(int64(appliedIndex))
	if err := engine.MVCCPut(ctx, eng, ms,
		rsl.RaftAppliedIndexKey(),
		hlc.Timestamp{},
		value,
		nil /* txn */); err != nil {
		return err
	}
	value.SetInt(int64(leaseAppliedIndex))
	return engine.MVCCPut(ctx, eng, ms,
		rsl.LeaseAppliedIndexKey(),
		hlc.Timestamp{},
		value,
		nil /* txn */)
}

// setAppliedIndexBlind sets the {raft,lease} applied index values using a
// "blind" put which ignores any existing keys. This is identical to
// setAppliedIndex but is used to optimize the writing of the applied index
// values during write operations where we definitively know the size of the
// previous values.
func (rsl replicaStateLoader) setAppliedIndexBlind(
	ctx context.Context,
	eng engine.ReadWriter,
	ms *enginepb.MVCCStats,
	appliedIndex,
	leaseAppliedIndex uint64,
) error {
	var value roachpb.Value
	value.SetInt(int64(appliedIndex))
	if err := engine.MVCCBlindPut(ctx, eng, ms,
		rsl.RaftAppliedIndexKey(),
		hlc.Timestamp{},
		value,
		nil /* txn */); err != nil {
		return err
	}
	value.SetInt(int64(leaseAppliedIndex))
	return engine.MVCCBlindPut(ctx, eng, ms,
		rsl.LeaseAppliedIndexKey(),
		hlc.Timestamp{},
		value,
		nil /* txn */)
}

func inlineValueIntEncodedSize(v int64) int {
	var value roachpb.Value
	value.SetInt(v)
	meta := enginepb.MVCCMetadata{RawBytes: value.RawBytes}
	return meta.Size()
}

// Calculate the size (MVCCStats.SysBytes) of the {raft,lease} applied index
// keys/values.
func (rsl replicaStateLoader) calcAppliedIndexSysBytes(
	appliedIndex, leaseAppliedIndex uint64,
) int64 {
	return int64(engine.MakeMVCCMetadataKey(rsl.RaftAppliedIndexKey()).EncodedSize() +
		engine.MakeMVCCMetadataKey(rsl.LeaseAppliedIndexKey()).EncodedSize() +
		inlineValueIntEncodedSize(int64(appliedIndex)) +
		inlineValueIntEncodedSize(int64(leaseAppliedIndex)))
}

func loadTruncatedState(
	ctx context.Context, reader engine.Reader, rangeID roachpb.RangeID,
) (roachpb.RaftTruncatedState, error) {
	rsl := makeReplicaStateLoader(rangeID)
	return rsl.loadTruncatedState(ctx, reader)
}

func (rsl replicaStateLoader) loadTruncatedState(
	ctx context.Context, reader engine.Reader,
) (roachpb.RaftTruncatedState, error) {
	var truncState roachpb.RaftTruncatedState
	if _, err := engine.MVCCGetProto(ctx, reader,
		rsl.RaftTruncatedStateKey(), hlc.Timestamp{}, true,
		nil, &truncState); err != nil {
		return roachpb.RaftTruncatedState{}, err
	}
	return truncState, nil
}

func (rsl replicaStateLoader) setTruncatedState(
	ctx context.Context,
	eng engine.ReadWriter,
	ms *enginepb.MVCCStats,
	truncState *roachpb.RaftTruncatedState,
) error {
	if (*truncState == roachpb.RaftTruncatedState{}) {
		return errors.New("cannot persist empty RaftTruncatedState")
	}
	return engine.MVCCPutProto(ctx, eng, ms,
		rsl.RaftTruncatedStateKey(), hlc.Timestamp{}, nil, truncState)
}

func (rsl replicaStateLoader) loadGCThreshold(
	ctx context.Context, reader engine.Reader,
) (hlc.Timestamp, error) {
	var t hlc.Timestamp
	_, err := engine.MVCCGetProto(ctx, reader, rsl.RangeLastGCKey(),
		hlc.Timestamp{}, true, nil, &t)
	return t, err
}

func (rsl replicaStateLoader) setGCThreshold(
	ctx context.Context, eng engine.ReadWriter, ms *enginepb.MVCCStats, threshold *hlc.Timestamp,
) error {
	if threshold == nil {
		return errors.New("cannot persist nil GCThreshold")
	}
	return engine.MVCCPutProto(ctx, eng, ms,
		rsl.RangeLastGCKey(), hlc.Timestamp{}, nil, threshold)
}

func (rsl replicaStateLoader) loadTxnSpanGCThreshold(
	ctx context.Context, reader engine.Reader,
) (hlc.Timestamp, error) {
	var t hlc.Timestamp
	_, err := engine.MVCCGetProto(ctx, reader, rsl.RangeTxnSpanGCThresholdKey(),
		hlc.Timestamp{}, true, nil, &t)
	return t, err
}

func (rsl replicaStateLoader) setTxnSpanGCThreshold(
	ctx context.Context, eng engine.ReadWriter, ms *enginepb.MVCCStats, threshold *hlc.Timestamp,
) error {
	if threshold == nil {
		return errors.New("cannot persist nil TxnSpanGCThreshold")
	}

	return engine.MVCCPutProto(ctx, eng, ms,
		rsl.RangeTxnSpanGCThresholdKey(), hlc.Timestamp{}, nil, threshold)
}

func (rsl replicaStateLoader) loadMVCCStats(
	ctx context.Context, reader engine.Reader,
) (enginepb.MVCCStats, error) {
	var ms enginepb.MVCCStats
	_, err := engine.MVCCGetProto(ctx, reader, rsl.RangeStatsKey(), hlc.Timestamp{}, true, nil, &ms)
	return ms, err
}

func (rsl replicaStateLoader) setMVCCStats(
	ctx context.Context, eng engine.ReadWriter, newMS *enginepb.MVCCStats,
) error {
	return engine.MVCCPutProto(ctx, eng, nil, rsl.RangeStatsKey(), hlc.Timestamp{}, nil, newMS)
}

func (rsl replicaStateLoader) setFrozenStatus(
	ctx context.Context,
	eng engine.ReadWriter,
	ms *enginepb.MVCCStats,
	frozen storagebase.ReplicaState_FrozenEnum,
) error {
	if frozen == storagebase.ReplicaState_FROZEN_UNSPECIFIED {
		return errors.New("cannot persist unspecified FrozenStatus")
	}
	var val roachpb.Value
	val.SetBool(frozen == storagebase.ReplicaState_FROZEN)
	return engine.MVCCPut(ctx, eng, ms, rsl.RangeFrozenStatusKey(), hlc.Timestamp{}, val, nil)
}

func (rsl replicaStateLoader) loadFrozenStatus(
	ctx context.Context, reader engine.Reader,
) (storagebase.ReplicaState_FrozenEnum, error) {
	var zero storagebase.ReplicaState_FrozenEnum
	val, _, err := engine.MVCCGet(ctx, reader, rsl.RangeFrozenStatusKey(),
		hlc.Timestamp{}, true, nil)
	if err != nil {
		return zero, err
	}
	if val == nil {
		return storagebase.ReplicaState_UNFROZEN, nil
	}
	if frozen, err := val.GetBool(); err != nil {
		return zero, err
	} else if frozen {
		return storagebase.ReplicaState_FROZEN, nil
	}
	return storagebase.ReplicaState_UNFROZEN, nil
}

// The rest is not technically part of ReplicaState.
// TODO(tschottdorf): more consolidation of ad-hoc structures: last index and
// hard state. These are closely coupled with ReplicaState (and in particular
// with its TruncatedState) but are different in that they are not consistently
// updated through Raft.

func loadLastIndex(
	ctx context.Context, reader engine.Reader, rangeID roachpb.RangeID,
) (uint64, error) {
	rsl := makeReplicaStateLoader(rangeID)
	return rsl.loadLastIndex(ctx, reader)
}

func (rsl replicaStateLoader) loadLastIndex(
	ctx context.Context, reader engine.Reader,
) (uint64, error) {
	var lastIndex uint64
	v, _, err := engine.MVCCGet(ctx, reader, rsl.RaftLastIndexKey(),
		hlc.Timestamp{}, true /* consistent */, nil)
	if err != nil {
		return 0, err
	}
	if v != nil {
		int64LastIndex, err := v.GetInt()
		if err != nil {
			return 0, err
		}
		lastIndex = uint64(int64LastIndex)
	} else {
		// The log is empty, which means we are either starting from scratch
		// or the entire log has been truncated away.
		lastEnt, err := rsl.loadTruncatedState(ctx, reader)
		if err != nil {
			return 0, err
		}
		lastIndex = lastEnt.Index
	}
	return lastIndex, nil
}

func (rsl replicaStateLoader) setLastIndex(
	ctx context.Context, eng engine.ReadWriter, lastIndex uint64,
) error {
	var value roachpb.Value
	value.SetInt(int64(lastIndex))
	return engine.MVCCPut(ctx, eng, nil, rsl.RaftLastIndexKey(),
		hlc.Timestamp{}, value, nil /* txn */)
}

// loadReplicaDestroyedError loads the replica destroyed error for the specified
// range. If there is no error, nil is returned.
func (rsl replicaStateLoader) loadReplicaDestroyedError(
	ctx context.Context, reader engine.Reader,
) (*roachpb.Error, error) {
	var v roachpb.Error
	found, err := engine.MVCCGetProto(ctx, reader,
		rsl.RangeReplicaDestroyedErrorKey(),
		hlc.Timestamp{}, true /* consistent */, nil, &v)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return &v, nil
}

// setReplicaDestroyedError sets an error indicating that the replica has been
// destroyed.
func (rsl replicaStateLoader) setReplicaDestroyedError(
	ctx context.Context, eng engine.ReadWriter, err *roachpb.Error,
) error {
	return engine.MVCCPutProto(ctx, eng, nil,
		rsl.RangeReplicaDestroyedErrorKey(), hlc.Timestamp{}, nil /* txn */, err)
}

func loadHardState(
	ctx context.Context, reader engine.Reader, rangeID roachpb.RangeID,
) (raftpb.HardState, error) {
	rsl := makeReplicaStateLoader(rangeID)
	return rsl.loadHardState(ctx, reader)
}

func (rsl replicaStateLoader) loadHardState(
	ctx context.Context, reader engine.Reader,
) (raftpb.HardState, error) {
	var hs raftpb.HardState
	found, err := engine.MVCCGetProto(ctx, reader,
		rsl.RaftHardStateKey(),
		hlc.Timestamp{}, true, nil, &hs)

	if !found || err != nil {
		return raftpb.HardState{}, err
	}
	return hs, nil
}

func (rsl replicaStateLoader) setHardState(
	ctx context.Context, batch engine.ReadWriter, st raftpb.HardState,
) error {
	return engine.MVCCPutProto(ctx, batch, nil,
		rsl.RaftHardStateKey(), hlc.Timestamp{}, nil, &st)
}

// synthesizeHardState synthesizes a HardState from the given ReplicaState and
// any existing on-disk HardState in the context of a snapshot, while verifying
// that the application of the snapshot does not violate Raft invariants. It
// must be called after the supplied state and ReadWriter have been updated
// with the result of the snapshot.
// If there is an existing HardState, we must respect it and we must not apply
// a snapshot that would move the state backwards.
func (rsl replicaStateLoader) synthesizeHardState(
	ctx context.Context, eng engine.ReadWriter, s storagebase.ReplicaState, oldHS raftpb.HardState,
) error {
	newHS := raftpb.HardState{
		Term: s.TruncatedState.Term,
		// Note that when applying a Raft snapshot, the applied index is
		// equal to the Commit index represented by the snapshot.
		Commit: s.RaftAppliedIndex,
	}

	if oldHS.Commit > newHS.Commit {
		return errors.Errorf("can't decrease HardState.Commit from %d to %d",
			oldHS.Commit, newHS.Commit)
	}
	if oldHS.Term > newHS.Term {
		// The existing HardState is allowed to be ahead of us, which is
		// relevant in practice for the split trigger. We already checked above
		// that we're not rewinding the acknowledged index, and we haven't
		// updated votes yet.
		newHS.Term = oldHS.Term
	}
	// If the existing HardState voted in this term, remember that.
	if oldHS.Term == newHS.Term {
		newHS.Vote = oldHS.Vote
	}
	err := rsl.setHardState(ctx, eng, newHS)
	return errors.Wrapf(err, "writing HardState %+v", &newHS)
}

// writeInitialState bootstraps a new Raft group (i.e. it is called when we
// bootstrap a Range, or when setting up the right hand side of a split).
// Its main task is to persist a consistent Raft (and associated Replica) state
// which does not start from zero but presupposes a few entries already having
// applied.
// The supplied MVCCStats are used for the Stats field after adjusting for
// persisting the state itself, and the updated stats are returned.
func writeInitialState(
	ctx context.Context,
	eng engine.ReadWriter,
	ms enginepb.MVCCStats,
	desc roachpb.RangeDescriptor,
	oldHS raftpb.HardState,
	lease *roachpb.Lease,
) (enginepb.MVCCStats, error) {
	rsl := makeReplicaStateLoader(desc.RangeID)

	var s storagebase.ReplicaState
	s.TruncatedState = &roachpb.RaftTruncatedState{
		Term:  raftInitialLogTerm,
		Index: raftInitialLogIndex,
	}
	s.RaftAppliedIndex = s.TruncatedState.Index
	s.Desc = &roachpb.RangeDescriptor{
		RangeID: desc.RangeID,
	}
	s.Frozen = storagebase.ReplicaState_UNFROZEN
	s.Stats = ms
	s.Lease = lease

	if existingLease, err := rsl.loadLease(ctx, eng); err != nil {
		return enginepb.MVCCStats{}, errors.Wrap(err, "error reading lease")
	} else if (existingLease != roachpb.Lease{}) {
		log.Fatalf(ctx, "expected trivial lease, but found %+v", existingLease)
	}

	newMS, err := rsl.save(ctx, eng, s)
	if err != nil {
		return enginepb.MVCCStats{}, err
	}

	if err := rsl.synthesizeHardState(ctx, eng, s, oldHS); err != nil {
		return enginepb.MVCCStats{}, err
	}

	if err := rsl.setLastIndex(ctx, eng, s.TruncatedState.Index); err != nil {
		return enginepb.MVCCStats{}, err
	}

	return newMS, nil
}
