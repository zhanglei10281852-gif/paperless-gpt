package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOCRExecutionRegistryFencing verifies the core reservation guarantees:
// only one holder per document, a rejected acquire identifies the current
// executor, and a late release of an old job can never remove a newer run.
func TestOCRExecutionRegistryFencing(t *testing.T) {
	r := newOCRExecutionRegistry()

	execA, holder, acquired := r.tryAcquire(context.Background(), 1, "job-A", "manual")
	require.True(t, acquired)
	require.NotNil(t, execA)

	// A second entry point is rejected and pointed at the actual executor.
	_, holder, acquired = r.tryAcquire(context.Background(), 1, "job-B", "auto")
	require.False(t, acquired)
	assert.Equal(t, "job-A", holder.JobID)
	assert.Equal(t, "manual", holder.Trigger)
	assert.Equal(t, 1, holder.DocumentID)

	// A different document is unaffected.
	execOther, _, acquiredOther := r.tryAcquire(context.Background(), 2, "job-other", "manual")
	require.True(t, acquiredOther)
	require.NotNil(t, execOther)

	// A ends and releases. B can now acquire.
	require.True(t, r.releaseJob("job-A"))
	execB, _, acquired := r.tryAcquire(context.Background(), 1, "job-B", "auto")
	require.True(t, acquired)

	// Late cleanup from the old run A must not release the newer run B:
	// neither the document reservation nor B's context may be touched.
	assert.False(t, r.releaseJob("job-A"))
	current, ok := r.current(1)
	require.True(t, ok)
	assert.Equal(t, "job-B", current.JobID)
	select {
	case <-execB.ctx.Done():
		t.Fatal("late release of job-A canceled the newer run B")
	default:
	}

	// Stop reaches the actual executor.
	require.True(t, r.cancelJob("job-B"))
	select {
	case <-execB.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("cancelJob did not cancel the run context")
	}
	require.True(t, r.releaseJob("job-B"))
	_, ok = r.current(1)
	assert.False(t, ok)

	require.True(t, r.releaseJob("job-other"))
}

// TestOCRExecutionRegistryCancelsWithParent ensures a background run whose
// poll loop ends (shutdown) has its context canceled, while the reservation
// itself is only dropped by an explicit release (the restart path relies on
// this: interrupted rows never block new runs because the table is in-memory).
func TestOCRExecutionRegistryCancelsWithParent(t *testing.T) {
	r := newOCRExecutionRegistry()
	parent, cancelParent := context.WithCancel(context.Background())
	exec, _, acquired := r.tryAcquire(parent, 9, "job-parent", "auto")
	require.True(t, acquired)

	cancelParent()
	select {
	case <-exec.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("canceling the parent context did not cancel the run")
	}

	// The reservation is still held until explicitly released.
	_, holder, acquired := r.tryAcquire(context.Background(), 9, "job-new", "manual")
	require.False(t, acquired)
	assert.Equal(t, "job-parent", holder.JobID)
	r.releaseJob("job-parent")
}

// TestSubmitOCRJobHandlerBusyConflict: two manual requests for the same
// document while the first is still active — the second gets a recognizable
// 409 pointing at the existing job; once the first finishes, a new run starts.
func TestSubmitOCRJobHandlerBusyConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newOCRRunTestDB(t)
	app := &App{Client: &mockPaperlessClient{}, Database: db}

	router := gin.New()
	router.POST("/api/documents/:id/ocr", app.submitOCRJobHandler)

	const docID = 9101
	post := func() (int, map[string]interface{}) {
		req, err := http.NewRequest(http.MethodPost, "/api/documents/9101/ocr", bytes.NewBufferString(`{}`))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		var body map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		return w.Code, body
	}
	// drainQueue empties jobs left in the global queue by earlier tests.
	drainQueue := func() int {
		n := 0
		for {
			select {
			case <-jobQueue:
				n++
			default:
				return n
			}
		}
	}
	drainAndRelease := func(jobID string) {
		drainQueue()
		ocrExecutions.releaseJob(jobID)
	}

	drainQueue()
	code, body := post()
	require.Equal(t, http.StatusAccepted, code)
	firstJobID, _ := body["job_id"].(string)
	require.NotEmpty(t, firstJobID)
	t.Cleanup(func() { drainAndRelease(firstJobID) })

	// Duplicate while the first run still owns the document.
	code, body = post()
	require.Equal(t, http.StatusConflict, code, "a busy document must return 409, not a second job")
	assert.Equal(t, ocrBusyCode, body["code"])
	assert.Equal(t, firstJobID, body["job_id"], "the busy response must identify the existing run")
	assert.EqualValues(t, docID, body["document_id"])

	// Exactly one job ever reached the queue: the busy request never enqueued.
	assert.Equal(t, 1, drainQueue())

	// After the first run ends, the document accepts a new run.
	ocrExecutions.releaseJob(firstJobID)
	code, body = post()
	require.Equal(t, http.StatusAccepted, code)
	secondJobID, _ := body["job_id"].(string)
	assert.NotEqual(t, firstJobID, secondJobID)
	t.Cleanup(func() { drainAndRelease(secondJobID) })
}

// busyTestApp builds the minimal App used by the background-poll tests.
func busyTestApp(t *testing.T, client *recordingClient, processor *controllableProcessor) *App {
	t.Helper()
	return &App{
		Client:         client,
		docProcessor:   processor,
		Database:       newOCRRunTestDB(t),
		ocrProcessMode: "image",
	}
}

// controllableProcessor is a DocumentProcessor mock that can block until its
// context is canceled (simulating a long OCR run) and then switch to healthy.
type controllableProcessor struct {
	mu          sync.Mutex
	calls       int
	block       bool
	started     chan struct{}
	startedOnce sync.Once
	err         error
	text        string
}

func (p *controllableProcessor) ProcessDocumentOCR(ctx context.Context, documentID int, options OCROptions, jobID string) (*ProcessedDocument, error) {
	p.mu.Lock()
	p.calls++
	block := p.block
	err := p.err
	text := p.text
	p.mu.Unlock()
	if block {
		p.startedOnce.Do(func() { close(p.started) })
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	return &ProcessedDocument{ID: documentID, Text: text}, nil
}

func (p *controllableProcessor) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// TestProcessAutoOcrDefersWhileBusy: while another entry point owns the
// document, the auto poll must keep the trigger tag and defer — without
// calling OCR, surfacing an error, or consuming a failure retry.
func TestProcessAutoOcrDefersWhileBusy(t *testing.T) {
	prevAutoOcrTag := autoOcrTag
	prevMaxRetries := ocrMaxRetries
	t.Cleanup(func() {
		autoOcrTag = prevAutoOcrTag
		ocrMaxRetries = prevMaxRetries
	})
	autoOcrTag = "paperless-gpt-ocr-auto"
	ocrMaxRetries = 3

	const docID = 9201
	client := &recordingClient{taggedDocuments: map[string][]Document{
		autoOcrTag: {{ID: docID, Title: "Long Doc", Tags: []string{autoOcrTag}}},
	}}
	processor := &controllableProcessor{text: "ocr text"}
	app := busyTestApp(t, client, processor)

	// Simulate a Playground run already executing the document.
	holderJobID := generateJobID()
	_, _, acquired := ocrExecutions.tryAcquire(context.Background(), docID, holderJobID, "manual")
	require.True(t, acquired)
	defer ocrExecutions.releaseJob(holderJobID)

	for round := 1; round <= ocrMaxRetries; round++ {
		count, err := app.processAutoOcrTagDocuments(context.Background())
		require.NoErrorf(t, err, "busy round %d must defer, not error", round)
		assert.Equal(t, 0, count, "busy round %d must not count as processed", round)
	}
	assert.Equal(t, 0, processor.callCount(), "the occupied document must never be OCRed by the poll")
	assert.Empty(t, client.calls, "the trigger tag must stay untouched (no PATCH) on deferral")

	// Once the holder releases, real failures must still start counting from
	// zero — the busy rounds spent no retry attempts: two failures (< limit 3)
	// must not trigger the fail-tag loop break yet.
	ocrExecutions.releaseJob(holderJobID)
	processor.err = context.DeadlineExceeded // any non-cancellation error
	for round := 1; round <= 2; round++ {
		_, err := app.processAutoOcrTagDocuments(context.Background())
		assert.Error(t, err)
	}
	assert.Empty(t, client.calls, "busy rounds must not have advanced the failure counter")
}

// TestProcessAutoOcrUserCancel: stopping the actual executor cancels only
// that run, records "cancelled" (never a failure), keeps the trigger tag,
// releases the document so it runs again on the next poll cycle.
func TestProcessAutoOcrUserCancel(t *testing.T) {
	prevAutoOcrTag := autoOcrTag
	prevMaxRetries := ocrMaxRetries
	t.Cleanup(func() {
		autoOcrTag = prevAutoOcrTag
		ocrMaxRetries = prevMaxRetries
	})
	autoOcrTag = "paperless-gpt-ocr-auto"
	ocrMaxRetries = 3

	const docID = 9301
	client := &recordingClient{taggedDocuments: map[string][]Document{
		autoOcrTag: {{ID: docID, Title: "Long Doc", Tags: []string{autoOcrTag}}},
	}}
	processor := &controllableProcessor{block: true, started: make(chan struct{}), text: "ocr text"}
	app := busyTestApp(t, client, processor)

	done := make(chan struct{})
	go func() {
		_, _ = app.processAutoOcrTagDocuments(context.Background())
		close(done)
	}()

	// Wait until the poll owns the execution, then stop the actual run via
	// the same call the HTTP stop endpoint uses.
	select {
	case <-processor.started:
	case <-time.After(5 * time.Second):
		t.Fatal("background OCR never started")
	}
	holder, ok := ocrExecutions.current(docID)
	require.True(t, ok, "the auto run must hold the document execution while running")
	assert.Equal(t, "auto", holder.Trigger)
	require.True(t, ocrExecutions.cancelJob(holder.JobID))

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("auto OCR loop did not return after cancel")
	}

	// The run is recorded as cancelled, not failed; the tag was not removed.
	run, err := GetOCRRunByJobID(app.Database, holder.JobID)
	require.NoError(t, err)
	assert.Equal(t, "cancelled", run.Status)
	assert.Empty(t, client.calls, "cancellation must keep the trigger tag (no PATCH)")

	job, exists := jobStore.getJob(holder.JobID)
	require.True(t, exists)
	assert.Equal(t, "cancelled", job.Status)

	// The document is free again — no interrupted/stale reservation — and the
	// next poll cycle processes it normally.
	_, stillBusy := ocrExecutions.current(docID)
	assert.False(t, stillBusy, "cancellation must release the execution")

	processor.mu.Lock()
	processor.block = false
	processor.mu.Unlock()
	count, err := app.processAutoOcrTagDocuments(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, count, "the document must run again after being cancelled")
}

// TestWorkflowOCRDefersWhileBusy: a workflow poll encountering an occupied
// document returns (nil, nil) — the caller skips this cycle with the workflow
// trigger tag in place, no OCR work and no failure bookkeeping.
func TestWorkflowOCRDefersWhileBusy(t *testing.T) {
	const docID = 9401
	client := &recordingClient{}
	processor := &controllableProcessor{text: "wf ocr text"}
	app := &App{Client: client, docProcessor: processor, Database: newOCRRunTestDB(t)}

	wfOn := true
	wf := WorkflowConfig{ID: "billing", EnableOCR: &wfOn}
	doc := Document{ID: docID, Title: "WF Doc", Tags: []string{"wf-billing"}}

	holderJobID := generateJobID()
	_, _, acquired := ocrExecutions.tryAcquire(context.Background(), docID, holderJobID, "manual")
	require.True(t, acquired)
	defer ocrExecutions.releaseJob(holderJobID)

	updated, err := app.runWorkflowDocumentOCR(context.Background(), doc, wf, "wf-billing", documentLogger(docID))
	require.NoError(t, err)
	assert.Nil(t, updated, "busy workflow OCR must defer with nil,nil")
	assert.Equal(t, 0, processor.callCount(), "the occupied document must not be OCRed")

	// Once free, the workflow run proceeds normally.
	ocrExecutions.releaseJob(holderJobID)
	updated, err = app.runWorkflowDocumentOCR(context.Background(), doc, wf, "wf-billing", documentLogger(docID))
	require.NoError(t, err)
	require.NotNil(t, updated)
	assert.Equal(t, 1, processor.callCount())
}
