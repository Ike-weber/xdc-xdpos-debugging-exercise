// Copyright 2022 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package rawdb

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
)

// ReadSkeletonSyncStatus retrieves the serialized sync status saved at shutdown.
func ReadSkeletonSyncStatus(db ethdb.KeyValueReader) []byte {
	data, _ := db.Get(skeletonSyncStatusKey)
	return data
}

// WriteSkeletonSyncStatus stores the serialized sync status to save at shutdown.
func WriteSkeletonSyncStatus(db ethdb.KeyValueWriter, status []byte) {
	if err := db.Put(skeletonSyncStatusKey, status); err != nil {
		log.Crit("Failed to store skeleton sync status", "err", err)
	}
}

// DeleteSkeletonSyncStatus deletes the serialized sync status saved at the last
// shutdown
func DeleteSkeletonSyncStatus(db ethdb.KeyValueWriter) {
	if err := db.Delete(skeletonSyncStatusKey); err != nil {
		log.Crit("Failed to remove skeleton sync status", "err", err)
	}
}

// ReadSkeletonHeader retrieves a block header from the skeleton sync store,
func ReadSkeletonHeader(db ethdb.KeyValueReader, number uint64) *types.Header {
	data, _ := db.Get(skeletonHeaderKey(number))
	if len(data) == 0 {
		return nil
	}
	header := new(types.Header)
	if err := rlp.DecodeBytes(data, header); err != nil {
		log.Error("Invalid skeleton header RLP", "number", number, "err", err)
		return nil
	}
	return header
}

// WriteSkeletonHeader stores a block header into the skeleton sync store.
func WriteSkeletonHeader(db ethdb.KeyValueWriter, header *types.Header) {
	data, err := rlp.EncodeToBytes(header)
	if err != nil {
		log.Crit("Failed to RLP encode header", "err", err)
	}
	key := skeletonHeaderKey(header.Number.Uint64())
	if err := db.Put(key, data); err != nil {
		log.Crit("Failed to store skeleton header", "err", err)
	}
}

// DeleteSkeletonHeader removes all block header data associated with a hash.
func DeleteSkeletonHeader(db ethdb.KeyValueWriter, number uint64) {
	if err := db.Delete(skeletonHeaderKey(number)); err != nil {
		log.Crit("Failed to delete skeleton header", "err", err)
	}
}

const (
	StateSyncUnknown  = uint8(0) // flags the state snap sync is unknown
	StateSyncRunning  = uint8(1) // flags the state snap sync is not completed yet
	StateSyncFinished = uint8(2) // flags the state snap sync is completed
)

// ReadSnapSyncStatusFlag retrieves the state snap sync status flag.
func ReadSnapSyncStatusFlag(db ethdb.KeyValueReader) uint8 {
	blob, err := db.Get(snapSyncStatusFlagKey)
	if err != nil || len(blob) != 1 {
		return StateSyncUnknown
	}
	return blob[0]
}

// WriteSnapSyncStatusFlag stores the state snap sync status flag into database.
func WriteSnapSyncStatusFlag(db ethdb.KeyValueWriter, flag uint8) {
	if err := db.Put(snapSyncStatusFlagKey, []byte{flag}); err != nil {
		log.Crit("Failed to store sync status flag", "err", err)
	}
}

// CheckpointSyncMetadata stores the checkpoint sync metadata for resume.
// P0 HARDENING: Persist checkpoint metadata to DB.
type CheckpointSyncMetadata struct {
	Number uint64      // Checkpoint block number
	Hash   common.Hash // Checkpoint block hash
	Root   common.Hash // Checkpoint state root
}

// ReadCheckpointSyncMetadata retrieves the checkpoint sync metadata from database.
func ReadCheckpointSyncMetadata(db ethdb.KeyValueReader) *CheckpointSyncMetadata {
	data, err := db.Get(checkpointSyncMetadataKey)
	if err != nil || len(data) == 0 {
		return nil
	}
	meta := new(CheckpointSyncMetadata)
	if err := rlp.DecodeBytes(data, meta); err != nil {
		log.Error("Invalid checkpoint sync metadata RLP", "err", err)
		return nil
	}
	return meta
}

// WriteCheckpointSyncMetadata stores the checkpoint sync metadata into database.
func WriteCheckpointSyncMetadata(db ethdb.KeyValueWriter, meta *CheckpointSyncMetadata) {
	if meta == nil {
		return
	}
	data, err := rlp.EncodeToBytes(meta)
	if err != nil {
		log.Crit("Failed to RLP encode checkpoint sync metadata", "err", err)
	}
	if err := db.Put(checkpointSyncMetadataKey, data); err != nil {
		log.Crit("Failed to store checkpoint sync metadata", "err", err)
	}
	log.Info("Persisted checkpoint sync metadata to DB", "number", meta.Number, "hash", meta.Hash.Hex()[:16])
}

// DeleteCheckpointSyncMetadata removes the checkpoint sync metadata from database.
func DeleteCheckpointSyncMetadata(db ethdb.KeyValueWriter) {
	if err := db.Delete(checkpointSyncMetadataKey); err != nil {
		log.Crit("Failed to delete checkpoint sync metadata", "err", err)
	}
}

// ReadCheckpointSyncNoState retrieves the checkpoint sync no-state flag from database.
// P0 FIX: Read persisted checkpointSyncNoState flag on startup.
func ReadCheckpointSyncNoState(db ethdb.KeyValueReader) bool {
	data, err := db.Get(checkpointSyncNoStateKey)
	if err != nil || len(data) == 0 {
		return false
	}
	return len(data) == 1 && data[0] == 1
}

// WriteCheckpointSyncNoState stores the checkpoint sync no-state flag into database.
// P0 FIX: Persist checkpointSyncNoState flag when checkpoint is inserted without state.
func WriteCheckpointSyncNoState(db ethdb.KeyValueWriter, flag bool) {
	var data []byte
	if flag {
		data = []byte{1}
	} else {
		data = []byte{0}
	}
	if err := db.Put(checkpointSyncNoStateKey, data); err != nil {
		log.Crit("Failed to store checkpoint sync no-state flag", "err", err)
	}
	log.Info("Persisted checkpoint sync no-state flag to DB", "flag", flag)
}

// DeleteCheckpointSyncNoState removes the checkpoint sync no-state flag from database.
func DeleteCheckpointSyncNoState(db ethdb.KeyValueWriter) {
	if err := db.Delete(checkpointSyncNoStateKey); err != nil {
		log.Crit("Failed to delete checkpoint sync no-state flag", "err", err)
	}
}

// ReadLowestInsertedHeader retrieves the lowest inserted header number from database.
// P1 HARDENING: Track lowest inserted header for resume support.
func ReadLowestInsertedHeader(db ethdb.KeyValueReader) uint64 {
	data, err := db.Get(lowestInsertedHeaderKey)
	if err != nil || len(data) == 0 {
		return 0
	}
	var number uint64
	if err := rlp.DecodeBytes(data, &number); err != nil {
		log.Error("Invalid lowest inserted header RLP", "err", err)
		return 0
	}
	return number
}

// WriteLowestInsertedHeader stores the lowest inserted header number into database.
func WriteLowestInsertedHeader(db ethdb.KeyValueWriter, number uint64) {
	data, err := rlp.EncodeToBytes(number)
	if err != nil {
		log.Crit("Failed to RLP encode lowest inserted header", "err", err)
	}
	if err := db.Put(lowestInsertedHeaderKey, data); err != nil {
		log.Crit("Failed to store lowest inserted header", "err", err)
	}
	log.Debug("Persisted lowest inserted header to DB", "number", number)
}

// DeleteLowestInsertedHeader removes the lowest inserted header number from database.
func DeleteLowestInsertedHeader(db ethdb.KeyValueWriter) {
	if err := db.Delete(lowestInsertedHeaderKey); err != nil {
		log.Crit("Failed to delete lowest inserted header", "err", err)
	}
}
