// Copyright 2014 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package batcheval

import (
	"bytes"
	"context"
	"fmt"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/storage/engine"
	"github.com/cockroachdb/cockroach/pkg/storage/engine/enginepb"
	"github.com/pkg/errors"
)

// ErrTransactionUnsupported is returned when a non-transactional command is
// evaluated in the context of a transaction.
var ErrTransactionUnsupported = errors.New("not supported within a transaction")

// VerifyTransaction runs sanity checks verifying that the transaction in the
// header and the request are compatible.
func VerifyTransaction(
	h roachpb.Header, args roachpb.Request, permittedStatuses ...roachpb.TransactionStatus,
) error {
	if h.Txn == nil {
		return errors.Errorf("no transaction specified to %s", args.Method())
	}
	if !bytes.Equal(args.Header().Key, h.Txn.Key) {
		return errors.Errorf("request key %s should match txn key %s", args.Header().Key, h.Txn.Key)
	}
	statusPermitted := false
	for _, s := range permittedStatuses {
		if h.Txn.Status == s {
			statusPermitted = true
			break
		}
	}
	if !statusPermitted {
		return roachpb.NewTransactionStatusError(
			fmt.Sprintf("cannot perform %s with txn status %v", args.Method(), h.Txn.Status),
		)
	}
	return nil
}

// WriteAbortSpanOnResolve returns true if the abort span must be written when
// the transaction with the given status is resolved.
func WriteAbortSpanOnResolve(status roachpb.TransactionStatus) bool {
	return status == roachpb.ABORTED
}

// SetAbortSpan clears any AbortSpan entry if poison is false.
// Otherwise, if poison is true, creates an entry for this transaction
// in the AbortSpan to prevent future reads or writes from
// spuriously succeeding on this range.
func SetAbortSpan(
	ctx context.Context,
	rec EvalContext,
	batch engine.ReadWriter,
	ms *enginepb.MVCCStats,
	txn enginepb.TxnMeta,
	poison bool,
) error {
	// Read the current state of the AbortSpan so we can detect when
	// no changes are needed. This can help us avoid unnecessary Raft
	// proposals.
	var curEntry roachpb.AbortSpanEntry
	exists, err := rec.AbortSpan().Get(ctx, batch, txn.ID, &curEntry)
	if err != nil {
		return err
	}

	if !poison {
		if !exists {
			return nil
		}
		return rec.AbortSpan().Del(ctx, batch, ms, txn.ID)
	}

	entry := roachpb.AbortSpanEntry{
		Key:       txn.Key,
		Timestamp: txn.Timestamp,
		Priority:  txn.Priority,
	}
	if exists && curEntry.Equal(entry) {
		return nil
	}
	// curEntry already escapes, so assign entry to curEntry and pass
	// that to Put instead of allowing entry to escape as well.
	curEntry = entry
	return rec.AbortSpan().Put(ctx, batch, ms, txn.ID, &curEntry)
}

// CanPushWithPriority returns true if the given pusher can push the pushee
// based on its priority.
func CanPushWithPriority(pusher, pushee *roachpb.Transaction) bool {
	return (pusher.Priority > enginepb.MinTxnPriority && pushee.Priority == enginepb.MinTxnPriority) ||
		(pusher.Priority == enginepb.MaxTxnPriority && pushee.Priority < pusher.Priority)
}

// CanCreateTxnRecord determines whether a transaction record can be created for
// the provided transaction. If not, the function will return an error. If so,
// the function may modify the provided transaction.
func CanCreateTxnRecord(rec EvalContext, txn *roachpb.Transaction) error {
	// Provide the transaction's epoch zero original timestamp as its minimum
	// timestamp. The transaction could not have written a transaction record
	// previously with a timestamp below this.
	epochZeroOrigTS, _ := txn.InclusiveTimeBounds()
	ok, minTS, reason := rec.CanCreateTxnRecord(txn.ID, txn.Key, epochZeroOrigTS)
	if !ok {
		return roachpb.NewTransactionAbortedError(reason)
	}
	txn.Timestamp.Forward(minTS)
	return nil
}

// SynthesizeTxnFromMeta creates a synthetic transaction object from the
// provided transaction metadata. The synthetic transaction is not meant to be
// persisted, but can serve as a representation of the transaction for outside
// observation. The function also checks whether it is possible for the
// transaction to ever create a transaction record in the future. If not, the
// returned transaction will be marked as ABORTED and it is safe to assume that
// the transaction record will never be written in the future.
func SynthesizeTxnFromMeta(rec EvalContext, txn enginepb.TxnMeta) roachpb.Transaction {
	// Construct the transaction object.
	synthTxnRecord := roachpb.TransactionRecord{
		TxnMeta: txn,
		Status:  roachpb.PENDING,
		// Set the LastHeartbeat timestamp to the intent's timestamp.
		// We use this as an indication of client activity.
		LastHeartbeat: txn.Timestamp,
	}

	// Determine whether the transaction record could ever actually be written
	// in the future. We provide the TxnMeta's timestamp (which we read from an
	// intent) as the upper bound on the transaction's minimum timestamp. This
	// may be greater than the transaction's actually original epoch-zero
	// timestamp, in which case we're subjecting ourselves to false positives
	// where we don't discover that a transaction is uncommittable, but never
	// false negatives where we think that a transaction is uncommittable even
	// when it's not and could later complete.
	ok, minTS, _ := rec.CanCreateTxnRecord(txn.ID, txn.Key, txn.Timestamp)
	if ok {
		// Forward the provisional commit timestamp by the minimum timestamp that
		// the transaction would be able to create a transaction record at.
		synthTxnRecord.Timestamp.Forward(minTS)
	} else {
		// Mark the transaction as ABORTED because it is uncommittable.
		synthTxnRecord.Status = roachpb.ABORTED
	}
	return synthTxnRecord.AsTransaction()
}
