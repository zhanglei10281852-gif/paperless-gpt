package main

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// ModificationHistory represents the schema of the modification_history table
type ModificationHistory struct {
	ID            uint    `gorm:"primaryKey"`                                              // Auto-incrementing primary key
	DocumentID    uint    `gorm:"not null"`                                                // Foreign key to documents table (if applicable)
	DateChanged   string  `gorm:"not null"`                                                // Date and time of modification
	ModField      string  `gorm:"size:255;not null;uniqueIndex:idx_commit_field,priority:2"` // Field being modified
	PreviousValue string  `gorm:"size:1048576"`                                            // Previous value of the field
	NewValue      string  `gorm:"size:1048576"`                                            // New value of the field
	Undone        bool    `gorm:"not null;default:false"`                                  // Whether the modification has been undone
	UndoneDate    string  `gorm:"default:null"`                                            // Date and time of undoing the modification
	CommitID      *string `gorm:"type:VARCHAR(36);uniqueIndex:idx_commit_field,priority:1"` // Submission this row belongs to (NULL for legacy rows)
}

// Document update submission statuses. A commit row is only written after
// paperless-ngx has accepted the PATCH (HTTP 200), so Status is effectively
// always commitStatusApplied; it exists to keep the state machine explicit
// and future-proof.
const (
	commitStatusApplied = "applied"

	// HistoryStatus values: whether every landed field of the commit has a
	// ModificationHistory row. pending rows are reconciled by the background
	// commit reconciler and by idempotent client retries.
	commitHistoryPending  = "pending"
	commitHistoryComplete = "complete"
)

// CommittedFieldChange is one field-level change that paperless-ngx confirmed
// for a committed document update. The slice stored on DocumentUpdateCommit is
// the reconciliation source of truth: exactly these ModificationHistory rows
// must exist for the commit.
type CommittedFieldChange struct {
	Field         string `json:"field"`
	PreviousValue string `json:"previous_value"`
	NewValue      string `json:"new_value"`
}

// DocumentUpdateCommit is the durable identity of one document submission:
// once paperless-ngx accepted the PATCH (HTTP 200), this row exists and the
// submission is treated as committed regardless of subsequent local history
// write failures. The reconciler fills missing ModificationHistory rows from
// ChangesJSON without ever contacting paperless-ngx again.
type DocumentUpdateCommit struct {
	CommitID          string    `gorm:"primaryKey;size:36"` // Client-supplied or server-generated UUID
	DocumentID        uint      `gorm:"not null;index"`
	IsUndo            bool      `gorm:"not null;default:false"`
	Status            string    `gorm:"size:32;not null"`         // commitStatusApplied
	HistoryStatus     string    `gorm:"size:32;not null;index"`   // commitHistoryPending | commitHistoryComplete
	ChangesJSON       string    `gorm:"type:TEXT;not null"`       // []CommittedFieldChange
	DroppedFieldsJSON string    `gorm:"type:TEXT"`                // []string dropped after paperless-ngx 400s ("" = none)
	Attempts          int       `gorm:"not null;default:0"`       // Reconciliation attempts
	CreatedAt         time.Time // Commit record created
	AppliedAt         time.Time // Remote PATCH confirmed
	UpdatedAt         time.Time // Last reconciliation state change
}

type OCRPageResult struct {
	ID             uint   `gorm:"primaryKey"`
	DocumentID     int    `gorm:"index;not null"`
	JobID          string `gorm:"index;size:64"` // OCR run this page belongs to ("" on pre-run legacy rows)
	PageIndex      int    `gorm:"not null"`
	Text           string `gorm:"size:1048576"`
	OcrLimitHit    bool
	GenerationInfo string `gorm:"type:TEXT"`
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// InitializeDB initializes the SQLite database and migrates the schema
func InitializeDB() *gorm.DB {
	// Ensure db directory exists
	dbDir := "db"
	if err := os.MkdirAll(dbDir, os.ModePerm); err != nil {
		log.Fatalf("Failed to create db directory: %v", err)
	}

	dbPath := filepath.Join(dbDir, "modification_history.db")

	// Connect to SQLite database
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}

	// Migrate the schema (create the tables if they don't exist)
	err = db.AutoMigrate(&ModificationHistory{}, &DocumentUpdateCommit{}, &OCRPageResult{}, &OCRRun{})
	if err != nil {
		log.Fatalf("Failed to migrate database schema: %v", err)
	}

	return db
}

// InsertModification inserts a new modification record into the database
func InsertModification(db *gorm.DB, record *ModificationHistory) error {
	log.Debugf("Passed modification record: %+v", record)
	record.DateChanged = time.Now().Format(time.RFC3339) // Set the DateChanged field to the current time
	log.Debugf("Inserting modification record: %+v", record)
	result := db.Create(&record) // GORM's Create method
	log.Debugf("Insertion result: %+v", result)
	return result.Error
}

// GetModification retrieves a modification record by its ID
func GetModification(db *gorm.DB, id uint) (*ModificationHistory, error) {
	var record ModificationHistory
	result := db.First(&record, id) // GORM's First method retrieves the first record matching the ID
	return &record, result.Error
}

// GetAllModifications retrieves all modification records from the database (deprecated - use GetPaginatedModifications instead)
func GetAllModifications(db *gorm.DB) ([]ModificationHistory, error) {
	var records []ModificationHistory
	result := db.Order("date_changed DESC").Find(&records)
	return records, result.Error
}

// GetPaginatedModifications retrieves a page of modification records with total count
func GetPaginatedModifications(db *gorm.DB, page int, pageSize int) ([]ModificationHistory, int64, error) {
	var records []ModificationHistory
	var total int64

	// Get total count
	if err := db.Model(&ModificationHistory{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	// Calculate offset
	offset := (page - 1) * pageSize

	// Get paginated records
	result := db.Order("date_changed DESC").
		Offset(offset).
		Limit(pageSize).
		Find(&records)

	return records, total, result.Error
}

// UndoModification marks a modification record as undone and sets the undo date
func SetModificationUndone(db *gorm.DB, record *ModificationHistory) error {
	record.Undone = true
	record.UndoneDate = time.Now().Format(time.RFC3339)
	result := db.Save(&record) // GORM's Save method
	return result.Error
}

// SaveSingleOcrPageResult saves or updates the OCR result for a single page of
// one run, including GenerationInfo as JSON.
func SaveSingleOcrPageResult(db *gorm.DB, docID int, jobID string, pageIdx int, text string, ocrLimitHit bool, generationInfoJSON string) error {
	var result OCRPageResult
	tx := db.Where("document_id = ? AND job_id = ? AND page_index = ?", docID, jobID, pageIdx).First(&result)
	if tx.Error == nil {
		result.Text = text
		result.OcrLimitHit = ocrLimitHit
		result.GenerationInfo = generationInfoJSON
		return db.Save(&result).Error
	} else if tx.Error != nil {
		log.Debugf("SaveSingleOcrPageResult: db.First error: %v (is gorm.ErrRecordNotFound: %v)", tx.Error, errors.Is(tx.Error, gorm.ErrRecordNotFound))
		if errors.Is(tx.Error, gorm.ErrRecordNotFound) {
			result = OCRPageResult{
				DocumentID:     docID,
				JobID:          jobID,
				PageIndex:      pageIdx,
				Text:           text,
				OcrLimitHit:    ocrLimitHit,
				GenerationInfo: generationInfoJSON,
			}
			return db.Create(&result).Error
		} else {
			log.Errorf("Unexpected DB error in SaveSingleOcrPageResult: %v", tx.Error)
			return tx.Error
		}
	}
	return nil
}

// GetOcrPageResults returns the stored pages of one run. An empty jobID means
// "the latest run that has pages" (which also covers legacy rows).
func GetOcrPageResults(db *gorm.DB, docID int, jobID string) ([]OCRPageResult, error) {
	if jobID == "" {
		jobID = LatestOCRRunJobID(db, docID)
	}
	var results []OCRPageResult
	tx := db.Where("document_id = ? AND job_id = ?", docID, jobID).Order("page_index ASC").Find(&results)
	return results, tx.Error
}

func UpdateOcrPageResult(db *gorm.DB, docID int, jobID string, pageIdx int, text string, ocrLimitHit bool, generationInfoJSON string) error {
	return SaveSingleOcrPageResult(db, docID, jobID, pageIdx, text, ocrLimitHit, generationInfoJSON)
}
