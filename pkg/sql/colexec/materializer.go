// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package colexec

import (
	"context"
	"fmt"

	"github.com/cockroachdb/cockroach/pkg/col/coldata"
	"github.com/cockroachdb/cockroach/pkg/sql/colexecbase"
	"github.com/cockroachdb/cockroach/pkg/sql/colexecbase/colexecerror"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/log"
)

// Materializer converts an Operator input into a execinfra.RowSource.
type Materializer struct {
	execinfra.ProcessorBase
	NonExplainable

	input colexecbase.Operator
	typs  []*types.T

	da sqlbase.DatumAlloc

	// runtime fields --

	// curIdx represents the current index into the column batch: the next row the
	// Materializer will emit.
	curIdx int
	// batch is the current Batch the Materializer is processing.
	batch coldata.Batch
	// colvecs is the unwrapped batch.
	colvecs []coldata.Vec
	// sel is the selection vector on the batch.
	sel []int

	// row is the memory used for the output row.
	row sqlbase.EncDatumRow

	// Fields to store the returned results of next() to be passed through an
	// adapter.
	outputRow      sqlbase.EncDatumRow
	outputMetadata *execinfrapb.ProducerMetadata

	// cancelFlow will return a function to cancel the context of the flow. It is
	// a function in order to be lazily evaluated, since the context cancellation
	// function is only available when Starting. This function differs from
	// ctxCancel in that it will cancel all components of the Materializer's flow,
	// including those started asynchronously.
	cancelFlow func() context.CancelFunc

	// closers is a slice of IdempotentClosers that should be Closed on
	// termination.
	closers []IdempotentCloser
}

const materializerProcName = "materializer"

// NewMaterializer creates a new Materializer processor which processes the
// columnar data coming from input to return it as rows.
// Arguments:
// - typs is the output types scheme.
// - metadataSourcesQueue are all of the metadata sources that are planned on
// the same node as the Materializer and that need to be drained.
// - outputStatsToTrace (when tracing is enabled) finishes the stats.
// - cancelFlow should return the context cancellation function that cancels
// the context of the flow (i.e. it is Flow.ctxCancel). It should only be
// non-nil in case of a root Materializer (i.e. not when we're wrapping a row
// source).
// NOTE: the constructor does *not* take in an execinfrapb.PostProcessSpec
// because we expect input to handle that for us.
func NewMaterializer(
	flowCtx *execinfra.FlowCtx,
	processorID int32,
	input colexecbase.Operator,
	typs []*types.T,
	output execinfra.RowReceiver,
	metadataSourcesQueue []execinfrapb.MetadataSource,
	toClose []IdempotentCloser,
	outputStatsToTrace func(),
	cancelFlow func() context.CancelFunc,
) (*Materializer, error) {
	m := &Materializer{
		input:   input,
		typs:    typs,
		row:     make(sqlbase.EncDatumRow, len(typs)),
		closers: toClose,
	}

	if err := m.ProcessorBase.Init(
		m,
		// input must have handled any post-processing itself, so we pass in
		// an empty post-processing spec.
		&execinfrapb.PostProcessSpec{},
		typs,
		flowCtx,
		processorID,
		output,
		nil, /* memMonitor */
		execinfra.ProcStateOpts{
			TrailingMetaCallback: func(ctx context.Context) []execinfrapb.ProducerMetadata {
				var trailingMeta []execinfrapb.ProducerMetadata
				for _, src := range metadataSourcesQueue {
					trailingMeta = append(trailingMeta, src.DrainMeta(ctx)...)
				}
				m.InternalClose()
				return trailingMeta
			},
		},
	); err != nil {
		return nil, err
	}
	m.FinishTrace = outputStatsToTrace
	m.cancelFlow = cancelFlow
	return m, nil
}

var _ execinfra.OpNode = &Materializer{}

// ChildCount is part of the exec.OpNode interface.
func (m *Materializer) ChildCount(verbose bool) int {
	return 1
}

// Child is part of the exec.OpNode interface.
func (m *Materializer) Child(nth int, verbose bool) execinfra.OpNode {
	if nth == 0 {
		return m.input
	}
	colexecerror.InternalError(fmt.Sprintf("invalid index %d", nth))
	// This code is unreachable, but the compiler cannot infer that.
	return nil
}

// Start is part of the execinfra.RowSource interface.
func (m *Materializer) Start(ctx context.Context) context.Context {
	m.input.Init()
	return m.ProcessorBase.StartInternal(ctx, materializerProcName)
}

// nextAdapter calls next() and saves the returned results in m. For internal
// use only. The purpose of having this function is to not create an anonymous
// function on every call to Next().
func (m *Materializer) nextAdapter() {
	m.outputRow, m.outputMetadata = m.next()
}

// next is the logic of Next() extracted in a separate method to be used by an
// adapter to be able to wrap the latter with a catcher.
func (m *Materializer) next() (sqlbase.EncDatumRow, *execinfrapb.ProducerMetadata) {
	if m.State == execinfra.StateRunning {
		if m.batch == nil || m.curIdx >= m.batch.Length() {
			// Get a fresh batch.
			m.batch = m.input.Next(m.Ctx)

			if m.batch.Length() == 0 {
				m.MoveToDraining(nil /* err */)
				return nil, m.DrainHelper()
			}
			m.curIdx = 0
			m.colvecs = m.batch.ColVecs()
			m.sel = m.batch.Selection()
		}
		rowIdx := m.curIdx
		if m.sel != nil {
			rowIdx = m.sel[m.curIdx]
		}
		m.curIdx++

		for colIdx, typ := range m.typs {
			m.row[colIdx].Datum = PhysicalTypeColElemToDatum(m.colvecs[colIdx], rowIdx, &m.da, typ)
		}
		// Note that there is no post-processing to be done in the
		// materializer, so we do not use ProcessRowHelper and emit the row
		// directly.
		return m.row, nil
	}
	return nil, m.DrainHelper()
}

// Next is part of the execinfra.RowSource interface.
func (m *Materializer) Next() (sqlbase.EncDatumRow, *execinfrapb.ProducerMetadata) {
	if err := colexecerror.CatchVectorizedRuntimeError(m.nextAdapter); err != nil {
		m.MoveToDraining(err)
		return nil, m.DrainHelper()
	}
	return m.outputRow, m.outputMetadata
}

// InternalClose helps implement the execinfra.RowSource interface.
func (m *Materializer) InternalClose() bool {
	if m.ProcessorBase.InternalClose() {
		if m.cancelFlow != nil {
			m.cancelFlow()()
		}
		for _, closer := range m.closers {
			if err := closer.IdempotentClose(m.Ctx); err != nil {
				if log.V(1) {
					log.Infof(m.Ctx, "error closing Closer: %v", err)
				}
			}
		}
		return true
	}
	return false
}

// ConsumerDone is part of the execinfra.RowSource interface.
func (m *Materializer) ConsumerDone() {
	// Materializer will move into 'draining' state, and after all the metadata
	// has been drained - as part of TrailingMetaCallback - InternalClose() will
	// be called which will cancel the flow.
	m.MoveToDraining(nil /* err */)
}

// ConsumerClosed is part of the execinfra.RowSource interface.
func (m *Materializer) ConsumerClosed() {
	m.InternalClose()
}
