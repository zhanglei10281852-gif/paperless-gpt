package main

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// setupCommitUpdateEnv wires a mock paperless-ngx with an empty catalog and a
// private SQLite database, and counts PATCH requests against document 1.
func setupCommitUpdateEnv(t *testing.T) (*testEnv, *gorm.DB, *int64) {
	t.Helper()
	env := newTestEnv(t)
	db := newCommitTestDB(t)

	env.setMockResponse("/api/tags/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"results":[],"next":null}`))
	})

	var patches int64
	env.setMockResponse("/api/documents/1/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			atomic.AddInt64(&patches, 1)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})

	return env, db, &patches
}

// titleUpdate is a minimal single-field suggestion for these commit tests.
func titleUpdate(commitID string) DocumentSuggestion {
	return DocumentSuggestion{
		ID:               1,
		OriginalDocument: Document{ID: 1, Title: "Old title"},
		SuggestedTitle:   "New title",
		CommitID:         commitID,
	}
}

// AC-1/AC-2: paperless-ngx accepted the PATCH, but the first local history
// insert fails. UpdateDocuments must still report success (pending), keep the
// durable commit with the landed field, and on a same-commit-id retry never
// PATCH paperless-ngx again while backfilling the missing history.
func TestUpdateDocuments_HistoryFailureAfterPatchStaysCommittedAndRetriesDedupe(t *testing.T) {
	env, db, patches := setupCommitUpdateEnv(t)
	defer env.teardown()

	var historyFailures int64 = 1 // the first history insert fails
	failCreateCallback(t, db, "modification_histories", &historyFailures)

	// First call: remote 200, local history write fails.
	result, err := env.client.UpdateDocuments(context.Background(), []DocumentSuggestion{titleUpdate("cf-1")}, db, false)
	require.NoError(t, err, "a local history failure after a confirmed PATCH must not be returned as an error")
	require.NotNil(t, result)
	require.Len(t, result.Documents, 1)
	assert.False(t, result.Documents[0].AlreadyCommitted)
	assert.Equal(t, commitHistoryPending, result.Documents[0].HistoryStatus)
	assert.Equal(t, int64(1), atomic.LoadInt64(patches))

	commit, err := getCommitByID(db, "cf-1")
	require.NoError(t, err)
	assert.Equal(t, commitStatusApplied, commit.Status)
	assert.Equal(t, commitHistoryPending, commit.HistoryStatus)
	changes := parseCommitChanges(commit)
	require.Len(t, changes, 1)
	assert.Equal(t, "title", changes[0].Field)
	assert.Equal(t, "Old title", changes[0].PreviousValue)
	assert.Equal(t, "New title", changes[0].NewValue)

	// Local DB recovered. Retry with the same commit id: no second PATCH, the
	// missing history row is backfilled, the commit completes.
	atomic.StoreInt64(&historyFailures, 0)
	result2, err := env.client.UpdateDocuments(context.Background(), []DocumentSuggestion{titleUpdate("cf-1")}, db, false)
	require.NoError(t, err)
	require.Len(t, result2.Documents, 1)
	assert.True(t, result2.Documents[0].AlreadyCommitted, "a retried commit must be recognized as already committed")
	assert.Equal(t, commitHistoryComplete, result2.Documents[0].HistoryStatus)
	assert.Equal(t, int64(1), atomic.LoadInt64(patches), "paperless-ngx must not be PATCHed twice for one commit")

	var historyRows []ModificationHistory
	require.NoError(t, db.Where("commit_id = ?", "cf-1").Find(&historyRows).Error)
	require.Len(t, historyRows, 1, "no duplicate history rows after backfill")
	assert.Equal(t, "title", historyRows[0].ModField)
}

// AC-2 fast path: a repeat submit of a fully committed update sends no PATCH
// at all and reports the commit complete.
func TestUpdateDocuments_FastPathSkipsRemoteForKnownCommit(t *testing.T) {
	env, db, patches := setupCommitUpdateEnv(t)
	defer env.teardown()

	result, err := env.client.UpdateDocuments(context.Background(), []DocumentSuggestion{titleUpdate("cf-fast")}, db, false)
	require.NoError(t, err)
	require.Len(t, result.Documents, 1)
	assert.Equal(t, commitHistoryComplete, result.Documents[0].HistoryStatus)
	assert.Equal(t, "cf-fast", result.Documents[0].CommitID)

	result2, err := env.client.UpdateDocuments(context.Background(), []DocumentSuggestion{titleUpdate("cf-fast")}, db, false)
	require.NoError(t, err)
	require.Len(t, result2.Documents, 1)
	assert.True(t, result2.Documents[0].AlreadyCommitted)
	assert.Equal(t, int64(1), atomic.LoadInt64(patches))

	// A server-generated commit id for callers that send none is also
	// reported back and persisted.
	result3, err := env.client.UpdateDocuments(context.Background(), []DocumentSuggestion{titleUpdate("")}, db, false)
	require.NoError(t, err)
	require.Len(t, result3.Documents, 1)
	assert.NotEmpty(t, result3.Documents[0].CommitID)
	assert.Equal(t, int64(2), atomic.LoadInt64(patches), "a fresh submit without commit id is a new PATCH")
}
