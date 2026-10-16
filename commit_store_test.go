package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// newCommitTestDB opens a private in-memory SQLite database migrated with the
// commit/history schema. A unique DSN per test keeps the shared in-memory
// cache from leaking rows between tests.
func newCommitTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:commit_%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&ModificationHistory{}, &DocumentUpdateCommit{}))
	return db
}

func sampleChanges() []CommittedFieldChange {
	return []CommittedFieldChange{
		{Field: "title", PreviousValue: "Old", NewValue: "New"},
		{Field: "tags", PreviousValue: `["a"]`, NewValue: `["a","b"]`},
		{Field: "content", PreviousValue: "x", NewValue: "y"},
	}
}

func TestPersistCommittedUpdate_CompleteWritesCommitAndHistory(t *testing.T) {
	db := newCommitTestDB(t)

	commit := newCommitRecord("abc-1", 42, false, sampleChanges(), nil, time.Now())
	status := persistCommittedUpdate(db, &commit, sampleChanges())
	require.Equal(t, commitHistoryComplete, status)

	stored, err := getCommitByID(db, "abc-1")
	require.NoError(t, err)
	assert.Equal(t, uint(42), stored.DocumentID)
	assert.Equal(t, commitStatusApplied, stored.Status)
	assert.Equal(t, commitHistoryComplete, stored.HistoryStatus)

	var rows []ModificationHistory
	require.NoError(t, db.Where("commit_id = ?", "abc-1").Order("mod_field ASC").Find(&rows).Error)
	require.Len(t, rows, 3)
	assert.Equal(t, "content", rows[0].ModField)
	assert.Equal(t, uint(42), rows[0].DocumentID)
	assert.Equal(t, []string{"content", "tags", "title"}, []string{rows[0].ModField, rows[1].ModField, rows[2].ModField})
}

func TestModificationHistory_CommitUniqueIndexAndNullLegacyRows(t *testing.T) {
	db := newCommitTestDB(t)

	// Any number of legacy rows with NULL commit_id coexist.
	for i := 0; i < 3; i++ {
		require.NoError(t, db.Create(&ModificationHistory{
			DocumentID:  7,
			DateChanged: time.Now().Format(time.RFC3339),
			ModField:    "title",
		}).Error)
	}

	commitID := "dup-1"
	first := ModificationHistory{DocumentID: 7, DateChanged: time.Now().Format(time.RFC3339), ModField: "tags", CommitID: &commitID}
	require.NoError(t, db.Create(&first).Error)
	second := ModificationHistory{DocumentID: 7, DateChanged: time.Now().Format(time.RFC3339), ModField: "tags", CommitID: &commitID}
	err := db.Create(&second).Error
	require.Error(t, err, "duplicate (commit_id, mod_field) must be rejected")

	// Same field under a different commit is allowed.
	otherID := "dup-2"
	require.NoError(t, db.Create(&ModificationHistory{
		DocumentID: 7, DateChanged: time.Now().Format(time.RFC3339), ModField: "tags", CommitID: &otherID,
	}).Error)
}

func TestCompleteCommitIfPossible_BackfillsMissingRowsIdempotently(t *testing.T) {
	db := newCommitTestDB(t)
	changes := sampleChanges()
	now := time.Now()
	commit := newCommitRecord("half-1", 9, false, changes, nil, now)
	commit.HistoryStatus = commitHistoryPending
	require.NoError(t, db.Create(&commit).Error)

	// Only 2 of 3 history rows made it (e.g. process interrupted).
	id := commit.CommitID
	for _, change := range changes[:2] {
		require.NoError(t, db.Create(&ModificationHistory{
			DocumentID:  9,
			DateChanged: now.Format(time.RFC3339),
			ModField:    change.Field,
			CommitID:    &id,
		}).Error)
	}

	status, found, err := completeCommitIfPossible(db, "half-1")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, commitHistoryComplete, status)

	var rows []ModificationHistory
	require.NoError(t, db.Where("commit_id = ?", "half-1").Find(&rows).Error)
	assert.Len(t, rows, 3)

	var rowCount int64
	db.Model(&ModificationHistory{}).Where("commit_id = ?", "half-1").Count(&rowCount)

	// Second pass changes nothing.
	status2, _, err := completeCommitIfPossible(db, "half-1")
	require.NoError(t, err)
	assert.Equal(t, commitHistoryComplete, status2)
	var rowCountAfter int64
	db.Model(&ModificationHistory{}).Where("commit_id = ?", "half-1").Count(&rowCountAfter)
	assert.Equal(t, rowCount, rowCountAfter)
}

// failCreateCallback injects failures into gorm Create for a specific table.
// failCount is decremented per matching Create; failures are injected while
// it is positive. A negative value means "fail forever".
func failCreateCallback(t *testing.T, db *gorm.DB, table string, failCount *int64) {
	t.Helper()
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register("test_fail_create_"+table, func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Table != table {
			return
		}
		remaining := atomic.LoadInt64(failCount)
		if remaining < 0 {
			tx.AddError(assert.AnError)
			return
		}
		if remaining > 0 {
			atomic.AddInt64(failCount, -1)
			tx.AddError(assert.AnError)
		}
	}))
}

func TestPersistCommittedUpdate_HistoryWriteFailureStaysPendingThenReconciles(t *testing.T) {
	db := newCommitTestDB(t)
	var failures int64 = -1 // fail every history insert
	failCreateCallback(t, db, "modification_histories", &failures)

	changes := sampleChanges()
	commit := newCommitRecord("pending-1", 11, false, changes, nil, time.Now())
	status := persistCommittedUpdate(db, &commit, changes)
	assert.Equal(t, commitHistoryPending, status, "remote success must not turn into an error")

	stored, err := getCommitByID(db, "pending-1")
	require.NoError(t, err)
	assert.Equal(t, commitHistoryPending, stored.HistoryStatus)

	// Recover the local database: reconciler backfills without re-PATCHing.
	atomic.StoreInt64(&failures, 0)
	n, err := ReconcilePendingCommits(context.Background(), db, 10)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, 1)

	stored, err = getCommitByID(db, "pending-1")
	require.NoError(t, err)
	assert.Equal(t, commitHistoryComplete, stored.HistoryStatus)
	var rows int64
	db.Model(&ModificationHistory{}).Where("commit_id = ?", "pending-1").Count(&rows)
	assert.Equal(t, int64(3), rows)
}

func TestPersistCommittedUpdate_CommitRowFailureQueuedAndReconciles(t *testing.T) {
	db := newCommitTestDB(t)
	for range commitQueue.drain() {
	} // isolate from any queued entries of earlier tests
	var failures int64 = -1 // fail the commit row inserts too
	failCreateCallback(t, db, "document_update_commits", &failures)

	changes := sampleChanges()
	commit := newCommitRecord("queued-1", 13, false, changes, nil, time.Now())
	status := persistCommittedUpdate(db, &commit, changes)
	assert.Equal(t, commitHistoryPending, status)
	assert.Equal(t, 1, commitQueue.len())
	_, err := getCommitByID(db, "queued-1")
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)

	atomic.StoreInt64(&failures, 0)
	n, err := ReconcilePendingCommits(context.Background(), db, 10)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, 1)
	assert.Equal(t, 0, commitQueue.len())

	stored, err := getCommitByID(db, "queued-1")
	require.NoError(t, err)
	assert.Equal(t, commitHistoryComplete, stored.HistoryStatus)
}

func TestStartCommitReconciler_StartupPassAndShutdown(t *testing.T) {
	db := newCommitTestDB(t)
	changes := sampleChanges()
	commit := newCommitRecord("startup-1", 21, false, changes, nil, time.Now())
	commit.HistoryStatus = commitHistoryPending
	require.NoError(t, db.Create(&commit).Error)

	ctx, cancel := context.WithCancel(context.Background())
	StartCommitReconciler(ctx, db, 50*time.Millisecond)

	require.Eventually(t, func() bool {
		stored, err := getCommitByID(db, "startup-1")
		return err == nil && stored.HistoryStatus == commitHistoryComplete
	}, 2*time.Second, 20*time.Millisecond)

	cancel()
	// Cancelled context must not be fatal and the goroutine exits without
	// panicking; the completed state persists.
	stored, err := getCommitByID(db, "startup-1")
	require.NoError(t, err)
	assert.Equal(t, commitHistoryComplete, stored.HistoryStatus)
}
