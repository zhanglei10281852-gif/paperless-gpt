package main

import (
	"context"
	"sync"
	"time"
)

// ocrBusyCode is the machine-readable error code returned when a manual OCR
// request hits a document that already has a valid executor. The frontend uses
// it (together with HTTP 409) to attach to the existing run instead of
// treating the response as a failure.
const ocrBusyCode = "ocr_run_in_progress"

// activeOCRExecution is the single valid executor of one document's OCR
// lifecycle: download, recognition, per-page result persistence, and the
// final PDF upload / version append / original replacement. While an
// execution exists for a document, no other entry point (manual Playground
// request, auto-OCR poll, workflow poll) may run OCR for that document.
//
// Executions are in-memory only. They cover queued manual jobs as well as
// running ones, and the table starts empty after a restart — stale
// in_progress OCRRun rows are marked interrupted then, so a new execution can
// start immediately.
type activeOCRExecution struct {
	documentID int
	jobID      string
	trigger    string // "manual", "auto", or "workflow:<id>"
	ctx        context.Context
	cancel     context.CancelFunc
	startedAt  time.Time
}

// OCRExecutionInfo is a lock-free snapshot describing the current executor of
// a document, safe to hand to HTTP responses and log lines.
type OCRExecutionInfo struct {
	DocumentID int       `json:"document_id"`
	JobID      string    `json:"job_id"`
	Trigger    string    `json:"trigger"`
	StartedAt  time.Time `json:"started_at"`
}

func (e *activeOCRExecution) snapshot() OCRExecutionInfo {
	return OCRExecutionInfo{
		DocumentID: e.documentID,
		JobID:      e.jobID,
		Trigger:    e.trigger,
		StartedAt:  e.startedAt,
	}
}

// ocrExecutionRegistry tracks at most one active execution per document.
// The zero value must not be used; construct with newOCRExecutionRegistry.
type ocrExecutionRegistry struct {
	mu      sync.Mutex
	byDoc   map[int]*activeOCRExecution
	byJobID map[string]*activeOCRExecution
}

func newOCRExecutionRegistry() *ocrExecutionRegistry {
	return &ocrExecutionRegistry{
		byDoc:   make(map[int]*activeOCRExecution),
		byJobID: make(map[string]*activeOCRExecution),
	}
}

// ocrExecutions is the process-wide execution table shared by every OCR entry
// point.
var ocrExecutions = newOCRExecutionRegistry()

// tryAcquire reserves documentID for jobID. On success the returned execution
// owns the document until releaseJob is called with the same jobID. Its
// context is canceled when the user stops the run or parent ends.
//
// On failure the document is already owned by another execution; the returned
// snapshot identifies that executor so callers can return a recognizable busy
// response (manual) or defer while keeping the trigger tag (background).
func (r *ocrExecutionRegistry) tryAcquire(parent context.Context, documentID int, jobID, trigger string) (*activeOCRExecution, OCRExecutionInfo, bool) {
	ctx, cancel := context.WithCancel(parent)
	exec := &activeOCRExecution{
		documentID: documentID,
		jobID:      jobID,
		trigger:    trigger,
		ctx:        ctx,
		cancel:     cancel,
		startedAt:  time.Now(),
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if holder, ok := r.byDoc[documentID]; ok {
		// Do not leak the unused context.
		cancel()
		return nil, holder.snapshot(), false
	}
	r.byDoc[documentID] = exec
	r.byJobID[jobID] = exec
	return exec, OCRExecutionInfo{}, true
}

// releaseJob surrenders the reservation held by jobID. It is fenced by job
// identity: a late cleanup from an old run only releases the document while
// the document is still owned by that same run. If a newer run acquired the
// document in the meantime, its reservation is left untouched.
func (r *ocrExecutionRegistry) releaseJob(jobID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	exec, ok := r.byJobID[jobID]
	if !ok {
		return false
	}
	delete(r.byJobID, jobID)
	if current := r.byDoc[exec.documentID]; current != nil && current.jobID == jobID {
		delete(r.byDoc, exec.documentID)
	}
	exec.cancel()
	return true
}

// contextForJob returns the context of the execution owned by jobID.
func (r *ocrExecutionRegistry) contextForJob(jobID string) (context.Context, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	exec, ok := r.byJobID[jobID]
	if !ok {
		return nil, false
	}
	return exec.ctx, true
}

// current returns a snapshot of the execution currently owning documentID.
func (r *ocrExecutionRegistry) current(documentID int) (OCRExecutionInfo, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	holder, ok := r.byDoc[documentID]
	if !ok {
		return OCRExecutionInfo{}, false
	}
	return holder.snapshot(), true
}

// cancelJob requests cancellation of the run owned by jobID. It works
// regardless of the run's trigger (manual, auto, workflow), so the stop
// endpoint always reaches the actual executor.
func (r *ocrExecutionRegistry) cancelJob(jobID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	exec, ok := r.byJobID[jobID]
	if !ok {
		return false
	}
	exec.cancel()
	return true
}
