// Package staging defines Go interfaces and Dolt SQL schemas for a raw
// financial data staging pipeline that sits between bank downloads and
// double-entry ledger posting.
//
// Design principles:
//   - Dolt SQL tables for durable, versioned storage of raw data
//   - Clear lifecycle: staged -> matched -> posted -> reconciled (or rejected)
//   - Deduplication by (source_system, external_id, account)
//   - Incremental imports — add new data without re-importing
//   - Source adapters abstract bank-specific schemas (Chase CSV, Cash App, OFX)
//   - Agent-friendly: list, propose, approve/reject, post operations
//
// This file is a design proposal. It references types from the ledger package
// (docs/go-interfaces-proposal.go) and defines new types for the staging layer.
package staging

import (
	"context"
	"io"
	"time"

	"github.com/shopspring/decimal"
)

// =========================================================================
// SQL Table Schemas (Dolt)
// =========================================================================

// The following SQL DDL defines the Dolt tables backing this pipeline.
// Dolt provides Git-like versioning, which gives us:
//   - Full audit trail of every import and categorization decision
//   - Branch-based "what if" categorization by the agent
//   - Diff-based review of agent decisions before committing to ledger
//
// -- Import Sources: tracks each connected financial source
// CREATE TABLE import_sources (
//     source_id       VARCHAR(64) PRIMARY KEY,  -- e.g. "chase-checking-1234"
//     source_system   VARCHAR(32) NOT NULL,      -- e.g. "chase", "cashapp", "ofx"
//     account         VARCHAR(255) NOT NULL,     -- ledger Account, e.g. "Assets:Bank:Chase:Checking"
//     display_name    VARCHAR(255),              -- human-friendly name
//     config_json     JSON,                      -- source-specific config (column mappings, etc.)
//     last_import_at  DATETIME,                  -- when last successful import ran
//     created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
//     UNIQUE KEY idx_system_account (source_system, account)
// );
//
// -- Import Runs: tracks each import execution for auditability
// CREATE TABLE import_runs (
//     run_id          VARCHAR(64) PRIMARY KEY,   -- UUID
//     source_id       VARCHAR(64) NOT NULL,
//     filename        VARCHAR(512),              -- original file path or URL
//     file_hash       VARCHAR(64),               -- SHA-256 of imported file
//     format          VARCHAR(32) NOT NULL,       -- "csv", "ofx", "json", "pdf"
//     records_total   INT NOT NULL DEFAULT 0,
//     records_new     INT NOT NULL DEFAULT 0,
//     records_skipped INT NOT NULL DEFAULT 0,     -- duplicates skipped
//     started_at      DATETIME NOT NULL,
//     finished_at     DATETIME,
//     status          ENUM('running','completed','failed') NOT NULL DEFAULT 'running',
//     error_message   TEXT,
//     FOREIGN KEY (source_id) REFERENCES import_sources(source_id)
// );
//
// -- Staged Records: raw financial data awaiting categorization and posting
// CREATE TABLE staged_records (
//     record_id       VARCHAR(64) PRIMARY KEY,   -- UUID
//     source_id       VARCHAR(64) NOT NULL,
//     run_id          VARCHAR(64) NOT NULL,
//     external_id     VARCHAR(255) NOT NULL,      -- bank's transaction ID
//     txn_date        DATE NOT NULL,
//     post_date       DATE,                       -- settlement date if different
//     amount          DECIMAL(16,6) NOT NULL,
//     currency        VARCHAR(10) NOT NULL DEFAULT 'USD',
//     description     TEXT NOT NULL,
//     raw_payee       VARCHAR(512),
//     raw_category    VARCHAR(255),               -- bank's own category if provided
//     raw_json        JSON,                       -- full original record for reference
//     status          ENUM('staged','matched','posted','reconciled','rejected') NOT NULL DEFAULT 'staged',
//     proposed_account VARCHAR(255),              -- agent's proposed contra-account
//     proposed_payee  VARCHAR(255),               -- agent's cleaned payee
//     proposed_narration TEXT,                    -- agent's proposed narration
//     confidence      DECIMAL(3,2),               -- agent confidence 0.00-1.00
//     matched_txn_id  VARCHAR(64),                -- link to ledger transaction once posted
//     reviewed_at     DATETIME,                   -- when agent/human reviewed
//     posted_at       DATETIME,                   -- when posted to ledger
//     reconciled_at   DATETIME,                   -- when reconciled with statement
//     rejection_reason TEXT,
//     created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
//     FOREIGN KEY (source_id) REFERENCES import_sources(source_id),
//     FOREIGN KEY (run_id) REFERENCES import_runs(run_id),
//     UNIQUE KEY idx_dedup (source_id, external_id)
// );
//
// -- Create indexes for common query patterns
// CREATE INDEX idx_staged_status ON staged_records(status);
// CREATE INDEX idx_staged_date ON staged_records(txn_date);
// CREATE INDEX idx_staged_source_date ON staged_records(source_id, txn_date);
//
// -- Categorization Rules: learned patterns for auto-categorization
// CREATE TABLE categorization_rules (
//     rule_id         VARCHAR(64) PRIMARY KEY,
//     pattern         VARCHAR(512) NOT NULL,      -- regex or substring match on description/payee
//     match_field     ENUM('description','payee','raw_category') NOT NULL DEFAULT 'description',
//     target_account  VARCHAR(255) NOT NULL,       -- contra-account to assign
//     target_payee    VARCHAR(255),                -- cleaned payee name
//     priority        INT NOT NULL DEFAULT 0,      -- higher = checked first
//     source_system   VARCHAR(32),                 -- NULL means applies to all sources
//     hit_count       INT NOT NULL DEFAULT 0,      -- times this rule matched
//     created_by      VARCHAR(64) NOT NULL,         -- "agent" or "human"
//     created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
// );

// =========================================================================
// Record Lifecycle (State Machine)
// =========================================================================

// RecordStatus represents the lifecycle state of a staged record.
type RecordStatus string

const (
	// StatusStaged: raw record imported, awaiting review.
	StatusStaged RecordStatus = "staged"

	// StatusMatched: agent has proposed a categorization (account, payee, narration).
	// The record has a proposed_account and optional proposed_payee.
	StatusMatched RecordStatus = "matched"

	// StatusPosted: categorization approved and transaction written to ledger.
	// matched_txn_id links to the ledger Transaction.
	StatusPosted RecordStatus = "posted"

	// StatusReconciled: posted transaction verified against bank statement balance.
	StatusReconciled RecordStatus = "reconciled"

	// StatusRejected: record excluded from posting (duplicate, garbage, etc.).
	StatusRejected RecordStatus = "rejected"
)

// Valid transitions:
//   staged    -> matched    (agent proposes categorization)
//   staged    -> rejected   (agent rejects as garbage/duplicate)
//   matched   -> staged     (agent or human resets categorization)
//   matched   -> posted     (categorization approved, written to ledger)
//   matched   -> rejected   (categorization rejected)
//   posted    -> reconciled (statement balance confirms)
//   posted    -> matched    (un-post: ledger txn reversed, needs re-categorization)

// =========================================================================
// Core Types
// =========================================================================

// StagedRecord represents a single raw financial record in the staging area.
type StagedRecord struct {
	RecordID   string
	SourceID   string
	RunID      string
	ExternalID string // bank's transaction ID

	TxnDate  time.Time
	PostDate *time.Time // settlement date, nil if same as TxnDate

	Amount      decimal.Decimal
	Currency    string // e.g. "USD"
	Description string
	RawPayee    string
	RawCategory string
	RawJSON     []byte // original record bytes

	Status RecordStatus

	// Agent-proposed categorization (populated during matching)
	ProposedAccount   string           // contra-account, e.g. "Expenses:Food:Restaurants"
	ProposedPayee     string           // cleaned payee name
	ProposedNarration string           // human-readable narration
	Confidence        *decimal.Decimal // 0.00-1.00, nil if not scored

	// Post-matching fields
	MatchedTxnID    string     // ledger transaction ID once posted
	ReviewedAt      *time.Time
	PostedAt        *time.Time
	ReconciledAt    *time.Time
	RejectionReason string

	CreatedAt time.Time
}

// ImportSource represents a connected financial data source.
type ImportSource struct {
	SourceID     string
	SourceSystem string // "chase", "cashapp", "ofx", "plaid"
	Account      string // ledger Account path
	DisplayName  string
	ConfigJSON   []byte
	LastImportAt *time.Time
	CreatedAt    time.Time
}

// ImportRun tracks a single import execution.
type ImportRun struct {
	RunID          string
	SourceID       string
	Filename       string
	FileHash       string // SHA-256
	Format         string // "csv", "ofx", "json", "pdf"
	RecordsTotal   int
	RecordsNew     int
	RecordsSkipped int // duplicates
	StartedAt      time.Time
	FinishedAt     *time.Time
	Status         string // "running", "completed", "failed"
	ErrorMessage   string
}

// CategorizationRule is a learned pattern for auto-categorizing staged records.
type CategorizationRule struct {
	RuleID        string
	Pattern       string // regex or substring
	MatchField    string // "description", "payee", "raw_category"
	TargetAccount string
	TargetPayee   string
	Priority      int
	SourceSystem  string // empty means all sources
	HitCount      int
	CreatedBy     string // "agent" or "human"
	CreatedAt     time.Time
}

// =========================================================================
// Source Adapter Interface
// =========================================================================

// RawRecord is an intermediate representation produced by a source adapter.
// It normalizes bank-specific formats into a common shape before staging.
type RawRecord struct {
	ExternalID  string          // bank's unique transaction identifier
	Date        time.Time       // transaction date
	PostDate    *time.Time      // settlement date if different
	Amount      decimal.Decimal // signed: negative = debit, positive = credit
	Currency    string
	Description string
	Payee       string // if the source provides a separate payee field
	Category    string // if the source provides its own categorization
	RawData     []byte // original record bytes (JSON-encoded)
}

// SourceAdapter knows how to parse raw financial data from a specific source
// into normalized RawRecords. This is the extension point for adding new bank
// parsers.
//
// Modeled after beancount's identify/extract/file pattern:
//   - Identify: can this adapter handle the given data?
//   - Extract:  parse the data into RawRecords
//   - Source:   return metadata about the source
type SourceAdapter interface {
	// Identify returns true if this adapter can parse the given data.
	// The filename hint and first bytes help with format detection.
	Identify(filename string, head []byte) bool

	// Extract parses raw financial data into normalized records.
	// The reader provides the full file content.
	Extract(ctx context.Context, r io.Reader, filename string) ([]RawRecord, error)

	// SourceSystem returns the adapter's source system identifier (e.g. "chase", "ofx").
	SourceSystem() string

	// SupportedFormats returns the file formats this adapter handles (e.g. ["csv", "ofx"]).
	SupportedFormats() []string
}

// =========================================================================
// Pipeline Operations (Agent-facing)
// =========================================================================

// StagingStore provides CRUD access to the staged records in Dolt.
type StagingStore interface {
	// --- Import operations ---

	// RegisterSource adds or updates an import source configuration.
	RegisterSource(ctx context.Context, source ImportSource) error

	// GetSource retrieves source configuration by ID.
	GetSource(ctx context.Context, sourceID string) (*ImportSource, error)

	// ListSources returns all registered import sources.
	ListSources(ctx context.Context) ([]ImportSource, error)

	// BeginImport creates a new import run and returns its ID.
	BeginImport(ctx context.Context, sourceID, filename, format, fileHash string) (runID string, err error)

	// StageRecords inserts raw records into the staging table.
	// Deduplicates by (source_id, external_id) — existing records are skipped.
	// Returns counts of new and skipped records.
	StageRecords(ctx context.Context, runID string, records []RawRecord) (newCount, skippedCount int, err error)

	// CompleteImport finalizes an import run with success/failure status.
	CompleteImport(ctx context.Context, runID string, err error) error

	// --- Query operations (agent reads) ---

	// ListStaged returns staged records matching the given filter.
	ListStaged(ctx context.Context, filter StagedFilter) ([]StagedRecord, error)

	// GetRecord retrieves a single staged record by ID.
	GetRecord(ctx context.Context, recordID string) (*StagedRecord, error)

	// --- Categorization operations (agent writes) ---

	// ProposeMatch sets the proposed categorization on a staged record,
	// transitioning it from staged -> matched.
	ProposeMatch(ctx context.Context, recordID string, proposal MatchProposal) error

	// ApproveAndPost approves a matched record's categorization, generates
	// the corresponding ledger Transaction, and transitions to posted.
	// Returns the generated transaction.
	ApproveAndPost(ctx context.Context, recordID string) (*PostedTransaction, error)

	// BatchApproveAndPost approves multiple matched records in a single operation.
	BatchApproveAndPost(ctx context.Context, recordIDs []string) ([]PostedTransaction, error)

	// RejectRecord marks a record as rejected with a reason.
	RejectRecord(ctx context.Context, recordID, reason string) error

	// ResetToStaged returns a matched record back to staged status.
	ResetToStaged(ctx context.Context, recordID string) error

	// Reconcile marks a posted record as reconciled (confirmed by statement balance).
	Reconcile(ctx context.Context, recordID string) error

	// --- Categorization rules ---

	// SaveRule creates or updates an auto-categorization rule.
	SaveRule(ctx context.Context, rule CategorizationRule) error

	// MatchRules applies all categorization rules against a staged record,
	// returning the best-matching rule (if any).
	MatchRules(ctx context.Context, record StagedRecord) (*CategorizationRule, error)

	// AutoCategorize runs rule-based matching on all staged records,
	// proposing categorizations where rules match.
	// Returns count of records matched.
	AutoCategorize(ctx context.Context) (int, error)
}

// StagedFilter controls which staged records to return from ListStaged.
type StagedFilter struct {
	Status       *RecordStatus // filter by status; nil means all
	SourceID     string        // filter by source; empty means all
	DateFrom     *time.Time    // inclusive lower bound on txn_date
	DateTo       *time.Time    // exclusive upper bound on txn_date
	SearchText   string        // substring match on description/payee
	MinAmount    *decimal.Decimal
	MaxAmount    *decimal.Decimal
	Limit        int // 0 means default (100)
	Offset       int
}

// MatchProposal is an agent's proposed categorization for a staged record.
type MatchProposal struct {
	Account    string          // contra-account, e.g. "Expenses:Food:Restaurants"
	Payee      string          // cleaned payee name
	Narration  string          // optional narration
	Confidence decimal.Decimal // 0.00-1.00
}

// PostedTransaction links a staged record to its generated ledger transaction.
type PostedTransaction struct {
	RecordID string
	TxnID    string    // ledger transaction identifier
	PostedAt time.Time
}

// =========================================================================
// Import Pipeline (orchestration)
// =========================================================================

// ImportPipeline orchestrates the end-to-end import flow:
//   1. Detect source format via SourceAdapter.Identify
//   2. Extract raw records via SourceAdapter.Extract
//   3. Stage records via StagingStore.StageRecords (with dedup)
//   4. Auto-categorize via StagingStore.AutoCategorize
type ImportPipeline interface {
	// Import runs the full import flow for a file from a registered source.
	// It identifies the appropriate adapter, extracts records, stages them,
	// and runs auto-categorization.
	Import(ctx context.Context, sourceID string, filename string, data io.Reader) (*ImportRun, error)

	// RegisterAdapter adds a source adapter to the pipeline's adapter registry.
	RegisterAdapter(adapter SourceAdapter)
}

// =========================================================================
// Agent Workflow Interface
// =========================================================================

// AgentWorkflow provides a high-level interface for the AI agent to interact
// with the staging pipeline. It combines StagingStore operations with
// agent-specific convenience methods.
type AgentWorkflow interface {
	// ReviewQueue returns staged and matched records needing agent attention,
	// ordered by date (oldest first).
	ReviewQueue(ctx context.Context, limit int) ([]StagedRecord, error)

	// SuggestCategorization uses the agent's reasoning (rules + LLM) to
	// propose an account categorization for a staged record.
	SuggestCategorization(ctx context.Context, recordID string) (*MatchProposal, error)

	// BatchSuggest proposes categorizations for multiple records at once.
	BatchSuggest(ctx context.Context, recordIDs []string) (map[string]*MatchProposal, error)

	// LearnFromApproval extracts a categorization rule from an approved
	// match, so similar future records are auto-categorized.
	LearnFromApproval(ctx context.Context, recordID string) error

	// ReconcileStatement compares posted records against a bank statement
	// balance, marking records as reconciled where they agree.
	ReconcileStatement(ctx context.Context, sourceID string, statementDate time.Time, statementBalance decimal.Decimal) ([]string, error)
}
