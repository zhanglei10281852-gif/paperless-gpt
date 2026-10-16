package main

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"gorm.io/gorm"
)

// commitWriteRetries is the number of synchronous attempts made when the
// commit row or a history row cannot be written to the local SQLite database
// right after a confirmed remote PATCH. Local write failures are typically
// transient (SQLITE_BUSY, brief filesystem problems); after these attempts
// the commit stays recoverable through the in-process queue and the
// reconciler instead of being reported as a failed update.
const commitWriteRetries = 3

// commitReconcileBatch limits how many pending commits one reconciler pass
// touches, so an interrupted service with a large backlog still serves
// traffic between passes.
const commitReconcileBatch = 100

// fieldHistoryOrder keeps history rows for one commit in a stable, human
// meaningful order instead of random map iteration. Unknown fields sort
// after the known ones, alphabetically.
var fieldHistoryOrder = map[string]int{
	"tags":           0,
	"title":          1,
	"correspondent":  2,
	"document_type":  3,
	"created_date":   4,
	"content":        5,
	"custom_fields":  6,
}

// commitBackfillQueue holds fully-built commit records whose row could not be
// persisted locally even after the synchronous retries. The reconciler keeps
// trying to write them for the lifetime of the process. The remote update is
// already committed at this point, so entries must never be dropped silently
// while the process lives.
type commitBackfillQueue struct {
	mu      sync.Mutex
	pending map[string]DocumentUpdateCommit
}

var commitQueue = &commitBackfillQueue{pending: make(map[string]DocumentUpdateCommit)}

func (q *commitBackfillQueue) add(commit DocumentUpdateCommit) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending[commit.CommitID] = commit
}

// drain returns and clears all queued commits. Entries that fail to persist
// are re-added by the caller, so the queue only shrinks on real progress.
func (q *commitBackfillQueue) drain() []DocumentUpdateCommit {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]DocumentUpdateCommit, 0, len(q.pending))
	for _, commit := range q.pending {
		out = append(out, commit)
	}
	q.pending = make(map[string]DocumentUpdateCommit)
	return out
}

func (q *commitBackfillQueue) reAdd(commit DocumentUpdateCommit) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending[commit.CommitID] = commit
}

func (q *commitBackfillQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending)
}

// sortFieldChanges orders changes deterministically for history inserts.
func sortFieldChanges(changes []CommittedFieldChange) {
	orderRank := func(field string) int {
		if rank, ok := fieldHistoryOrder[field]; ok {
			return rank
		}
		return len(fieldHistoryOrder)
	}
	// Stable insertion sort — slices are small (≤ field count).
	for i := 1; i < len(changes); i++ {
		for j := i; j > 0; j-- {
			a, b := orderRank(changes[j-1].Field), orderRank(changes[j].Field)
			if a < b || (a == b && changes[j-1].Field < changes[j].Field) {
				break
			}
			changes[j-1], changes[j] = changes[j], changes[j-1]
		}
	}
}

func marshalFieldChanges(changes []CommittedFieldChange) string {
	if changes == nil {
		changes = []CommittedFieldChange{}
	}
	b, err := json.Marshal(changes)
	if err != nil {
		// All members are plain strings; marshalling cannot realistically fail.
		return "[]"
	}
	return string(b)
}

func marshalDroppedFields(dropped []string) string {
	if len(dropped) == 0 {
		return ""
	}
	b, err := json.Marshal(dropped)
	if err != nil {
		return ""
	}
	return string(b)
}

// newCommitRecord builds the durable identity for one submission confirmed by
// paperless-ngx. Timestamps are set by the caller path; this only fills the
// immutable identity and payload fields.
func newCommitRecord(commitID string, documentID uint, isUndo bool, changes []CommittedFieldChange, droppedFields []string, now time.Time) DocumentUpdateCommit {
	return DocumentUpdateCommit{
		CommitID:          commitID,
		DocumentID:        documentID,
		IsUndo:            isUndo,
		Status:            commitStatusApplied,
		HistoryStatus:     commitHistoryPending,
		ChangesJSON:       marshalFieldChanges(changes),
		DroppedFieldsJSON: marshalDroppedFields(droppedFields),
		CreatedAt:         now,
		AppliedAt:         now,
		UpdatedAt:         now,
	}
}

// withLocalRetries runs op up to commitWriteRetries times with a short
// backoff. SQLite write hiccups (busy/locked) usually clear within
// milliseconds; op decides what counts as an attempt.
func withLocalRetries(op func(attempt int) error) error {
	var err error
	for attempt := 1; attempt <= commitWriteRetries; attempt++ {
		if err = op(attempt); err == nil {
			return nil
		}
		if attempt < commitWriteRetries {
			time.Sleep(time.Duration(attempt) * 50 * time.Millisecond)
		}
	}
	return err
}

// insertCommitRow stores the commit record itself, retrying transient local
// failures. Callers must not treat a returned error as a remote failure: the
// PATCH has already been accepted by paperless-ngx.
func insertCommitRow(db *gorm.DB, commit *DocumentUpdateCommit) error {
	return withLocalRetries(func(attempt int) error {
		if attempt > 1 {
			log.Warnf("Retrying local commit record insert for document %d (attempt %d/%d): previous failure", commit.DocumentID, attempt, commitWriteRetries)
		}
		return db.Create(commit).Error
	})
}

// historyRowExists reports whether a history row already covers one field of a
// commit. The (commit_id, mod_field) unique index makes the check safe against
// duplicate reconciliation.
func historyRowExists(db *gorm.DB, commitID, field string) (bool, error) {
	var count int64
	if err := db.Model(&ModificationHistory{}).
		Where("commit_id = ? AND mod_field = ?", commitID, field).
		Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// insertHistoryRow writes one field row tagged with the commit id. A unique
// constraint violation (a concurrent writer won the race) is treated as
// success rather than an error.
func insertHistoryRow(db *gorm.DB, documentID uint, commitID string, change CommittedFieldChange) error {
	exists, err := historyRowExists(db, commitID, change.Field)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	commitIDCopy := commitID
	record := ModificationHistory{
		DocumentID:    documentID,
		ModField:      change.Field,
		PreviousValue: change.PreviousValue,
		NewValue:      change.NewValue,
		CommitID:      &commitIDCopy,
	}
	if err := InsertModification(db, &record); err != nil {
		// Re-check existence: another writer may have inserted between the
		// pre-check and the Create.
		if exists, existsErr := historyRowExists(db, commitID, change.Field); existsErr == nil && exists {
			return nil
		}
		return err
	}
	return nil
}

// ensureCommitHistory makes sure every change in the commit has a history
// row. Failures of individual rows do not abort the others; the returned
// slice lists changes that are still missing so the caller keeps the commit
// pending for the reconciler.
func ensureCommitHistory(db *gorm.DB, commit *DocumentUpdateCommit, changes []CommittedFieldChange) []CommittedFieldChange {
	var missing []CommittedFieldChange
	for _, change := range changes {
		if err := insertHistoryRow(db, commit.DocumentID, commit.CommitID, change); err != nil {
			log.Warnf("Document %d: history row for field %q (commit %s) could not be written locally: %v", commit.DocumentID, change.Field, commit.CommitID, err)
			missing = append(missing, change)
		}
	}
	return missing
}

// parseCommitChanges decodes the stored change list. A corrupt/empty payload
// means there is nothing to reconcile; the commit can be marked complete.
func parseCommitChanges(commit *DocumentUpdateCommit) []CommittedFieldChange {
	var changes []CommittedFieldChange
	if commit.ChangesJSON == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(commit.ChangesJSON), &changes); err != nil {
		log.Errorf("Commit %s (document %d) has an unreadable field-change payload %q: %v", commit.CommitID, commit.DocumentID, commit.ChangesJSON, err)
		return nil
	}
	return changes
}

// reconcileCommit compares the commit's stored change list with the history
// rows that actually exist, inserts the missing ones, and flips the commit to
// complete when every field is covered. It never contacts paperless-ngx and
// is safe to run repeatedly (startup, timer, duplicate client submit).
func reconcileCommit(db *gorm.DB, commit *DocumentUpdateCommit) string {
	changes := parseCommitChanges(commit)
	missing := ensureCommitHistory(db, commit, changes)

	now := time.Now()
	if len(missing) == 0 {
		if commit.HistoryStatus != commitHistoryComplete {
			commit.HistoryStatus = commitHistoryComplete
			commit.UpdatedAt = now
			if err := db.Model(&DocumentUpdateCommit{}).
				Where("commit_id = ?", commit.CommitID).
				Updates(map[string]interface{}{"history_status": commitHistoryComplete, "updated_at": now}).Error; err != nil {
				log.Warnf("Commit %s (document %d) history is complete but marking it failed: %v", commit.CommitID, commit.DocumentID, err)
			}
		}
		return commitHistoryComplete
	}

	commit.Attempts++
	commit.UpdatedAt = now
	commit.HistoryStatus = commitHistoryPending
	if err := db.Model(&DocumentUpdateCommit{}).
		Where("commit_id = ?", commit.CommitID).
		Updates(map[string]interface{}{"history_status": commitHistoryPending, "attempts": commit.Attempts, "updated_at": now}).Error; err != nil {
		log.Warnf("Commit %s (document %d) could not update reconciliation state: %v", commit.CommitID, commit.DocumentID, err)
	}
	return commitHistoryPending
}

// getCommitByID loads one commit record.
func getCommitByID(db *gorm.DB, commitID string) (*DocumentUpdateCommit, error) {
	var commit DocumentUpdateCommit
	if err := db.Where("commit_id = ?", commitID).First(&commit).Error; err != nil {
		return nil, err
	}
	return &commit, nil
}

// completeCommitIfPossible reconciles a single commit by ID. found=false
// means there is no local record for the id yet.
func completeCommitIfPossible(db *gorm.DB, commitID string) (status string, found bool, err error) {
	commit, err := getCommitByID(db, commitID)
	if err != nil {
		return "", false, err
	}
	return reconcileCommit(db, commit), true, nil
}

// persistCommittedUpdate stores the commit confirmed by paperless-ngx and as
// many history rows as the local database currently accepts. It never returns
// an error to the caller: a persistent local failure moves the fully-built
// record into the in-process backfill queue and the result stays "pending".
// The returned status is complete or pending.
func persistCommittedUpdate(db *gorm.DB, commit *DocumentUpdateCommit, changes []CommittedFieldChange) string {
	sortFieldChanges(changes)
	if commit.ChangesJSON == "" {
		commit.ChangesJSON = marshalFieldChanges(changes)
	}
	if err := insertCommitRow(db, commit); err != nil {
		log.Errorf("Document %d: remote update committed (commit %s) but the local commit record could not be saved after %d attempts: %v. The change will be reconciled from the in-process queue; paperless-ngx already accepted it.", commit.DocumentID, commit.CommitID, commitWriteRetries, err)
		commit.HistoryStatus = commitHistoryPending
		commitQueue.add(*commit)
		return commitHistoryPending
	}

	missing := ensureCommitHistory(db, commit, changes)
	if len(missing) > 0 {
		log.Warnf("Document %d: remote update committed (commit %s); %d of %d history row(s) are still missing and will be backfilled.", commit.DocumentID, commit.CommitID, len(missing), len(changes))
		commit.Attempts++
		commit.UpdatedAt = time.Now()
		_ = db.Model(&DocumentUpdateCommit{}).Where("commit_id = ?", commit.CommitID).
			Updates(map[string]interface{}{"attempts": commit.Attempts, "updated_at": commit.UpdatedAt}).Error
		return commitHistoryPending
	}

	if err := db.Model(&DocumentUpdateCommit{}).Where("commit_id = ?", commit.CommitID).
		Updates(map[string]interface{}{"history_status": commitHistoryComplete, "updated_at": time.Now()}).Error; err != nil {
		log.Warnf("Document %d: history rows written for commit %s but marking the commit complete failed: %v", commit.DocumentID, commit.CommitID, err)
		return commitHistoryPending
	}
	commit.HistoryStatus = commitHistoryComplete
	return commitHistoryComplete
}

// ReconcilePendingCommits first retries commit records stuck in the
// in-process queue, then fills missing history rows for commits persisted
// with history_status=pending. It writes only to the local database and is
// safe to invoke concurrently with request handling.
func ReconcilePendingCommits(ctx context.Context, db *gorm.DB, batchSize int) (int, error) {
	if batchSize <= 0 {
		batchSize = commitReconcileBatch
	}
	touched := 0

	// 1) Commit rows that could not be written at all when the PATCH returned.
	for _, queued := range commitQueue.drain() {
		if ctx.Err() != nil {
			commitQueue.reAdd(queued)
			return touched, ctx.Err()
		}
		commit := queued
		if existing, err := getCommitByID(db, commit.CommitID); err == nil {
			// Another path persisted it meanwhile — just reconcile.
			reconcileCommit(db, existing)
			touched++
			continue
		}
		if err := insertCommitRow(db, &commit); err != nil {
			log.Warnf("Commit %s (document %d) still cannot be persisted locally: %v; keeping it queued.", commit.CommitID, commit.DocumentID, err)
			commitQueue.reAdd(commit)
			continue
		}
		reconcileCommit(db, &commit)
		touched++
	}

	// 2) Persisted commits whose history is incomplete (crash mid-write,
	// earlier transient DB outage, ...).
	var pending []DocumentUpdateCommit
	if err := db.Where("history_status = ?", commitHistoryPending).
		Order("created_at ASC").
		Limit(batchSize).
		Find(&pending).Error; err != nil {
		return touched, err
	}
	for i := range pending {
		if ctx.Err() != nil {
			return touched, ctx.Err()
		}
		reconcileCommit(db, &pending[i])
		touched++
	}
	return touched, nil
}

// StartCommitReconciler runs one reconciliation pass immediately and then on
// every tick until ctx is cancelled. Failures are logged; they never stop the
// loop because the next pass retries everything that is still pending.
func StartCommitReconciler(ctx context.Context, db *gorm.DB, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	run := func(pass string) {
		n, err := ReconcilePendingCommits(ctx, db, commitReconcileBatch)
		if err != nil {
			if ctx.Err() == nil {
				log.Warnf("Commit history reconciliation pass %s failed: %v", pass, err)
			}
			return
		}
		if n > 0 {
			log.Infof("Commit history reconciliation pass %s processed %d commit(s).", pass, n)
		}
	}
	run("startup")
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run("periodic")
			}
		}
	}()
}
