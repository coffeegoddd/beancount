// Package acquisition defines Go interfaces for financial data acquisition —
// the mechanism of connecting to a user's bank/financial service and
// downloading their transaction data.
//
// This layer sits BEFORE the staging pipeline (staging-pipeline-design.go).
// Its sole job is getting raw financial data out of banks and into our system.
// It does NOT parse file formats, categorize transactions, or post to a ledger.
//
// Supported acquisition methods:
//   1. Plaid API — OAuth link flow, REST API to fetch transactions
//   2. OFX Direct Connect — HTTP POST with username/password to bank's OFX server
//   3. App-specific APIs — Cash App, Venmo, etc. (REST/GraphQL)
//   4. Open Banking (PSD2/UK) — regulated bank APIs with OAuth2
//
// Design principles:
//   - Single interface (Provider) abstracts ALL acquisition methods
//   - Auth flow differences are handled by the AuthFlow type + InitiateLink/CompleteLink
//   - Incremental sync via opaque cursor (Plaid sync cursor, OFX date range, etc.)
//   - Common data types: RawTransaction, AccountInfo, Balance
//   - Credential storage is delegated to a CredentialStore interface
//   - Provider implementations are registered in a ProviderRegistry
package acquisition

import (
	"context"
	"time"

	"github.com/shopspring/decimal"
)

// =========================================================================
// Common Data Types
// =========================================================================
//
// These types represent the normalized output of ALL acquisition methods.
// Every provider — Plaid, OFX, Cash App, Open Banking — must produce data
// in these shapes. The types deliberately capture what's COMMON across all
// sources, with RawJSON preserving provider-specific details.

// RawTransaction is a single financial transaction as reported by the source.
// This is the primary unit of data flowing from acquisition into staging.
type RawTransaction struct {
	// ExternalID is the provider's unique identifier for this transaction.
	// Plaid: transaction_id, OFX: FITID, Cash App: payment_id, etc.
	// Used for deduplication in the staging layer.
	ExternalID string

	// AccountID is the provider's identifier for the account this transaction
	// belongs to. Maps to a LinkedAccount.ProviderAccountID.
	AccountID string

	// Date is when the transaction occurred (not necessarily when it settled).
	Date time.Time

	// PostDate is the settlement date, if different from Date. Nil if same or unknown.
	PostDate *time.Time

	// Amount is the transaction amount. Sign convention: negative = money left
	// the account (debit/purchase), positive = money entered (credit/deposit).
	Amount decimal.Decimal

	// Currency is the ISO 4217 currency code (e.g. "USD", "GBP").
	Currency string

	// Description is the raw transaction description as provided by the bank.
	// May include merchant name, reference numbers, location — varies by source.
	Description string

	// Payee is the counterparty name if the source provides it separately
	// from the description. Empty if not available (OFX often doesn't have this).
	Payee string

	// Category is the source's own categorization, if provided.
	// Plaid provides detailed categories, OFX does not, Cash App has basic ones.
	Category string

	// Pending indicates whether the transaction has settled.
	// Plaid and some Open Banking APIs distinguish pending from posted.
	// OFX transactions are always settled (Pending = false).
	Pending bool

	// MerchantName is the cleaned merchant name if the source provides it.
	// Plaid provides this; most other sources do not.
	MerchantName string

	// RawJSON preserves the complete original record from the provider.
	// Enables re-extraction if our normalization logic changes.
	RawJSON []byte
}

// AccountInfo describes a financial account available at a linked institution.
// Returned by ListAccounts after a user links their institution.
type AccountInfo struct {
	// ProviderAccountID is the provider's unique identifier for this account.
	ProviderAccountID string

	// Name is the account's display name (e.g. "Plaid Checking", "My Savings").
	Name string

	// OfficialName is the institution's official name for the account, if available.
	OfficialName string

	// Type classifies the account (checking, savings, credit, investment, etc.).
	Type AccountType

	// Subtype provides finer classification (e.g. "money market", "cd").
	Subtype string

	// Mask is the last 4 digits of the account number (for display).
	Mask string

	// Currency is the account's primary currency.
	Currency string

	// InstitutionID is the provider's identifier for the institution.
	InstitutionID string

	// InstitutionName is the human-readable institution name.
	InstitutionName string
}

// AccountType classifies financial accounts at a high level.
type AccountType string

const (
	AccountTypeChecking   AccountType = "checking"
	AccountTypeSavings    AccountType = "savings"
	AccountTypeCredit     AccountType = "credit"
	AccountTypeLoan       AccountType = "loan"
	AccountTypeInvestment AccountType = "investment"
	AccountTypeOther      AccountType = "other"
)

// Balance represents an account balance at a point in time.
type Balance struct {
	// AccountID is the provider's identifier for the account.
	AccountID string

	// Current is the current balance (real-time or as of last sync).
	Current decimal.Decimal

	// Available is the available balance (current minus holds/pending).
	// Nil if the source doesn't distinguish available from current.
	Available *decimal.Decimal

	// Limit is the credit limit for credit accounts. Nil for non-credit accounts.
	Limit *decimal.Decimal

	// Currency is the balance currency.
	Currency string

	// AsOf is when this balance was reported.
	AsOf time.Time
}

// =========================================================================
// Sync Mechanism
// =========================================================================
//
// Incremental sync is critical — we never want to re-download an entire
// transaction history on each fetch. But each provider handles this differently:
//
//   - Plaid: opaque sync cursor (transactions/sync endpoint)
//   - OFX: date range queries (DTSTART/DTEND in request)
//   - Cash App: paginated API with cursor/offset
//   - Open Banking: date range or continuation token (varies by bank)
//
// We unify these behind an opaque SyncCursor that each provider interprets
// internally. The caller stores it and passes it back on the next fetch.

// SyncCursor is an opaque token representing the sync state for a linked account.
// Each provider encodes its own sync mechanism into this cursor:
//   - Plaid: the sync cursor string from transactions/sync
//   - OFX: JSON-encoded last-fetched date
//   - App APIs: pagination cursor or last-seen transaction ID
//   - Open Banking: continuation token or last-fetched date
//
// The cursor is stored by the caller (in CredentialStore alongside the link)
// and passed back on each subsequent FetchTransactions call.
type SyncCursor []byte

// SyncResult is the result of an incremental transaction fetch.
type SyncResult struct {
	// Added contains new transactions since the last sync.
	Added []RawTransaction

	// Modified contains transactions that changed since the last sync
	// (e.g. pending -> posted, amount adjusted). Only Plaid reliably
	// provides this; other providers return nil.
	Modified []RawTransaction

	// Removed contains ExternalIDs of transactions that were removed
	// since the last sync. Only Plaid reliably provides this.
	Removed []string

	// NextCursor is the updated sync cursor to store for the next fetch.
	// Always non-nil on success — even if no new transactions, the cursor
	// advances to avoid re-fetching the same window.
	NextCursor SyncCursor

	// HasMore indicates whether there are more transactions available
	// in the current sync window. If true, call FetchTransactions again
	// with NextCursor to get the next page.
	HasMore bool
}

// =========================================================================
// Authentication / Linking
// =========================================================================
//
// The biggest difference between acquisition methods is how users authenticate:
//
//   - Plaid: Browser-based Link flow. Server creates a link_token, client opens
//     Plaid Link UI, user authenticates, client receives a public_token,
//     server exchanges it for an access_token.
//
//   - OFX: Username + password + optional MFA. No browser needed. Credentials
//     are sent directly to the bank's OFX server via HTTP POST.
//
//   - App APIs: Varies. Cash App uses OAuth2; Venmo uses OAuth2 + device approval.
//
//   - Open Banking: OAuth2 consent flow with bank redirect (similar to Plaid but
//     standards-based: UK Open Banking, PSD2 in EU).
//
// We model this as a two-phase flow: InitiateLink (get instructions/URL) and
// CompleteLink (finalize with user-provided credentials/tokens).

// AuthFlow describes the type of authentication required to link an account.
type AuthFlow string

const (
	// AuthFlowRedirect requires redirecting the user to a URL (Plaid Link,
	// OAuth2 consent). InitiateLink returns a URL; CompleteLink receives
	// the callback token/code.
	AuthFlowRedirect AuthFlow = "redirect"

	// AuthFlowCredential requires direct username/password entry.
	// InitiateLink may return MFA challenges; CompleteLink receives
	// the credentials and MFA responses.
	AuthFlowCredential AuthFlow = "credential"

	// AuthFlowToken requires an API key or pre-existing access token.
	// Used for services where the user provides their own API credentials.
	AuthFlowToken AuthFlow = "token"
)

// LinkRequest contains the information needed to initiate account linking.
type LinkRequest struct {
	// InstitutionID identifies the financial institution to link.
	// Provider-specific: Plaid institution ID, OFX FI URL, etc.
	InstitutionID string

	// UserID is an opaque identifier for the user initiating the link.
	// Used to scope credentials and link tokens.
	UserID string

	// RedirectURI is where to send the user after OAuth/Plaid Link completion.
	// Required for AuthFlowRedirect, ignored for other flows.
	RedirectURI string

	// Products specifies which data products to request access for.
	// Plaid-specific (e.g. "transactions", "auth", "balance").
	// Ignored by providers that don't support product scoping.
	Products []string
}

// LinkResponse is the result of initiating a link.
type LinkResponse struct {
	// AuthFlow indicates what authentication the user must perform.
	AuthFlow AuthFlow

	// LinkURL is the URL to redirect the user to (for AuthFlowRedirect).
	// Empty for credential/token flows.
	LinkURL string

	// LinkToken is a short-lived token used by the client-side link component.
	// Plaid: the link_token passed to Plaid Link. Empty for non-Plaid flows.
	LinkToken string

	// MFAChallenge describes any MFA challenge the user must answer
	// (for AuthFlowCredential). Nil if no MFA or for redirect flows.
	MFAChallenge *MFAChallenge

	// SessionID is an opaque identifier for this link session.
	// Must be passed back in CompleteLink.
	SessionID string
}

// MFAChallenge represents a multi-factor authentication challenge.
type MFAChallenge struct {
	// Type describes the MFA method: "question", "code_sms", "code_email",
	// "code_app", "device_approval".
	Type string

	// Prompt is the question or instruction to display to the user.
	// e.g. "What is your mother's maiden name?" or "Enter the code sent to ***-1234"
	Prompt string

	// Choices are the available options for selection-type MFA.
	// Empty for free-text responses.
	Choices []string
}

// LinkCompletion contains the user's response to complete the link flow.
type LinkCompletion struct {
	// SessionID from the LinkResponse.
	SessionID string

	// PublicToken is the token received from the redirect callback.
	// Plaid: the public_token from Plaid Link success callback.
	// OAuth: the authorization code from the redirect.
	PublicToken string

	// Credentials for AuthFlowCredential.
	Username string
	Password string

	// MFAResponse is the user's response to an MFA challenge.
	MFAResponse string

	// APIKey for AuthFlowToken.
	APIKey string
}

// LinkedAccount represents a successfully linked financial account.
type LinkedAccount struct {
	// LinkID is our internal identifier for this link (UUID).
	LinkID string

	// ProviderID identifies which Provider manages this link.
	ProviderID string

	// InstitutionID is the provider's institution identifier.
	InstitutionID string

	// InstitutionName is the human-readable institution name.
	InstitutionName string

	// Accounts are the financial accounts available through this link.
	Accounts []AccountInfo

	// CreatedAt is when the link was established.
	CreatedAt time.Time

	// LastSyncAt is when transactions were last fetched.
	LastSyncAt *time.Time

	// Status indicates the link's health.
	Status LinkStatus

	// StatusDetail provides human-readable detail about the status
	// (e.g. "Credentials expired — user must re-authenticate").
	StatusDetail string
}

// LinkStatus indicates the health of a linked account.
type LinkStatus string

const (
	// LinkStatusActive means the link is healthy and can fetch data.
	LinkStatusActive LinkStatus = "active"

	// LinkStatusDegraded means some functionality is impaired but data
	// may still be available (e.g. balance works but transactions don't).
	LinkStatusDegraded LinkStatus = "degraded"

	// LinkStatusStale means the link's credentials have expired or been
	// revoked. The user must re-authenticate.
	LinkStatusStale LinkStatus = "stale"

	// LinkStatusRevoked means the user explicitly disconnected the link
	// at the institution or in our system.
	LinkStatusRevoked LinkStatus = "revoked"
)

// =========================================================================
// Provider Interface
// =========================================================================
//
// Provider is the central abstraction. Each acquisition method (Plaid, OFX,
// Cash App, Open Banking) implements this interface.
//
// What's COMMON across all implementations:
//   - They all produce RawTransactions with dates, amounts, descriptions
//   - They all have some concept of linked accounts
//   - They all support fetching transactions and balances
//
// What's DIFFERENT (handled by the interface design):
//   - Auth flows: redirect vs credential vs token (AuthFlow enum)
//   - Incremental sync: cursor vs date range (opaque SyncCursor)
//   - Pending transactions: some support it, some don't (Pending field)
//   - Modified/removed transactions: only Plaid reliably provides this
//   - Account types: different granularity per provider

// Provider abstracts a single financial data acquisition method.
type Provider interface {
	// --- Identity ---

	// ID returns the unique identifier for this provider (e.g. "plaid", "ofx",
	// "cashapp", "openbanking-uk").
	ID() string

	// DisplayName returns a human-readable name (e.g. "Plaid", "OFX Direct Connect").
	DisplayName() string

	// AuthFlowType returns the authentication flow this provider uses.
	AuthFlowType() AuthFlow

	// --- Linking ---

	// InitiateLink begins the account linking process.
	// Returns instructions (URL, MFA challenge, etc.) for the user.
	InitiateLink(ctx context.Context, req LinkRequest) (*LinkResponse, error)

	// CompleteLink finalizes account linking after the user authenticates.
	// Returns the linked account with available sub-accounts.
	CompleteLink(ctx context.Context, completion LinkCompletion) (*LinkedAccount, error)

	// RefreshLink attempts to refresh an expired or degraded link
	// without requiring full re-authentication. Not all providers support this.
	// Returns ErrReauthRequired if the user must go through the full link flow again.
	RefreshLink(ctx context.Context, linkID string) error

	// UnlinkAccount removes the link and revokes any stored credentials/tokens.
	UnlinkAccount(ctx context.Context, linkID string) error

	// --- Data Fetching ---

	// FetchTransactions performs an incremental sync of transactions for the
	// given account. Pass nil cursor for the initial fetch.
	//
	// Implementation notes per provider:
	//   - Plaid: calls /transactions/sync with the cursor
	//   - OFX: sends STMTTRNRQ with DTSTART derived from cursor
	//   - App APIs: calls paginated transaction list endpoint
	//   - Open Banking: calls account transactions endpoint with date/cursor params
	FetchTransactions(ctx context.Context, linkID string, accountID string, cursor SyncCursor) (*SyncResult, error)

	// FetchBalances returns current balances for all accounts under a link.
	FetchBalances(ctx context.Context, linkID string) ([]Balance, error)

	// ListAccounts returns the accounts available through the given link.
	// Useful for re-discovering accounts after the initial link (e.g. user
	// opened a new account at the same institution).
	ListAccounts(ctx context.Context, linkID string) ([]AccountInfo, error)

	// --- Health ---

	// CheckLinkHealth verifies that a link is still functional.
	// Updates the link's Status and StatusDetail.
	CheckLinkHealth(ctx context.Context, linkID string) (*LinkedAccount, error)
}

// ErrReauthRequired is returned when a link's credentials have expired
// and the user must go through the full link flow again.
// Wraps the underlying provider error for debugging.
type ErrReauthRequired struct {
	LinkID   string
	Provider string
	Reason   string
}

func (e *ErrReauthRequired) Error() string {
	return "reauthentication required for " + e.Provider + " link " + e.LinkID + ": " + e.Reason
}

// ErrRateLimited is returned when the provider's rate limit is exceeded.
type ErrRateLimited struct {
	RetryAfter time.Duration
}

func (e *ErrRateLimited) Error() string {
	return "rate limited, retry after " + e.RetryAfter.String()
}

// =========================================================================
// Credential Store
// =========================================================================
//
// Credentials (access tokens, passwords, API keys) must be stored securely.
// This interface decouples the Provider from the storage mechanism.
// Implementations might use:
//   - Encrypted Dolt table
//   - OS keychain (macOS Keychain, GNOME Keyring)
//   - Environment variables (for CI/testing)
//   - External vault (HashiCorp Vault, AWS Secrets Manager)

// CredentialStore persists and retrieves credentials for linked accounts.
type CredentialStore interface {
	// StoreCredential saves or updates a credential for a link.
	// The credential is an opaque blob — the Provider serializes/deserializes it.
	StoreCredential(ctx context.Context, linkID string, credential []byte) error

	// GetCredential retrieves the stored credential for a link.
	// Returns nil, nil if no credential is stored (not an error).
	GetCredential(ctx context.Context, linkID string) ([]byte, error)

	// DeleteCredential removes the stored credential for a link.
	DeleteCredential(ctx context.Context, linkID string) error

	// StoreSyncCursor saves the sync cursor for a (link, account) pair.
	StoreSyncCursor(ctx context.Context, linkID string, accountID string, cursor SyncCursor) error

	// GetSyncCursor retrieves the stored sync cursor.
	// Returns nil, nil if no cursor is stored (initial sync).
	GetSyncCursor(ctx context.Context, linkID string, accountID string) (SyncCursor, error)
}

// =========================================================================
// Provider Registry
// =========================================================================
//
// The registry manages available providers and routes operations to the
// correct implementation based on provider ID or institution.

// ProviderRegistry manages the set of available acquisition providers.
type ProviderRegistry interface {
	// Register adds a provider to the registry.
	Register(provider Provider)

	// Get returns the provider with the given ID.
	// Returns nil if not registered.
	Get(providerID string) Provider

	// List returns all registered providers.
	List() []Provider

	// FindForInstitution returns providers that can connect to the given
	// institution. Multiple providers may support the same institution
	// (e.g. Chase is available via both Plaid and OFX).
	FindForInstitution(institutionID string) []Provider
}

// =========================================================================
// Acquisition Service (Orchestration)
// =========================================================================
//
// The AcquisitionService ties together Provider, CredentialStore, and the
// staging pipeline. It's the entry point for the agent or CLI to:
//   - Link a new institution
//   - Sync transactions from a linked account
//   - Check health of all links

// AcquisitionService orchestrates financial data acquisition across providers.
type AcquisitionService interface {
	// --- Linking ---

	// LinkInstitution starts the process of connecting to a financial institution.
	// Selects the appropriate provider (or uses the specified one) and initiates linking.
	LinkInstitution(ctx context.Context, req LinkRequest, preferredProviderID string) (*LinkResponse, error)

	// CompleteLinking finalizes the link after user authentication.
	CompleteLinking(ctx context.Context, completion LinkCompletion) (*LinkedAccount, error)

	// ListLinks returns all active linked accounts.
	ListLinks(ctx context.Context, userID string) ([]LinkedAccount, error)

	// RemoveLink disconnects a linked institution and cleans up credentials.
	RemoveLink(ctx context.Context, linkID string) error

	// --- Sync ---

	// SyncAccount fetches new transactions for a specific linked account
	// and stages them in the staging pipeline.
	// Returns the count of new, modified, and removed transactions.
	SyncAccount(ctx context.Context, linkID string, accountID string) (*SyncSummary, error)

	// SyncAll fetches new transactions for ALL linked accounts.
	// Returns a summary per account.
	SyncAll(ctx context.Context, userID string) ([]SyncSummary, error)

	// --- Health ---

	// CheckHealth verifies all links are functional and reports any that
	// need re-authentication.
	CheckHealth(ctx context.Context, userID string) ([]LinkedAccount, error)

	// RefreshStaleLinks attempts to refresh any links that have gone stale.
	// Returns links that could not be refreshed (need user re-auth).
	RefreshStaleLinks(ctx context.Context, userID string) ([]LinkedAccount, error)
}

// SyncSummary reports the result of syncing a single account.
type SyncSummary struct {
	LinkID      string
	AccountID   string
	AccountName string
	ProviderID  string

	Added    int // new transactions staged
	Modified int // transactions updated in staging
	Removed  int // transactions removed from staging

	// Error is non-nil if the sync failed. A failed sync for one account
	// does not prevent syncing other accounts.
	Error error
}

// =========================================================================
// Implementation Notes
// =========================================================================
//
// How each provider would implement the interface:
//
// --- Plaid ---
//   ID():              "plaid"
//   AuthFlowType():    AuthFlowRedirect
//   InitiateLink():    POST /link/token/create -> returns link_token in LinkResponse.LinkToken
//   CompleteLink():    POST /item/public_token/exchange -> gets access_token, stores in CredentialStore
//   FetchTransactions: POST /transactions/sync with cursor -> maps to SyncResult directly
//                      (Plaid's sync API was designed for exactly this pattern)
//   SyncCursor:        Plaid's opaque cursor string
//   FetchBalances:     POST /accounts/balance/get
//   RefreshLink:       Not needed — Plaid manages token refresh internally
//   Notes:             Plaid is the cleanest fit. Their sync API maps 1:1 to our interface.
//
// --- OFX Direct Connect ---
//   ID():              "ofx"
//   AuthFlowType():    AuthFlowCredential
//   InitiateLink():    Returns SessionID, may return MFA challenge from bank
//   CompleteLink():    Tests credentials by making an OFX request; stores username/password
//   FetchTransactions: Sends STMTTRNRQ with DTSTART from cursor (last fetch date).
//                      Returns only Added (no Modified/Removed — OFX doesn't support diff sync).
//                      SyncCursor: JSON{"last_date":"2024-01-15"}
//   FetchBalances:     Parses LEDGERBAL/AVAILBAL from OFX STMTRS response
//   RefreshLink:       Re-validates credentials; returns ErrReauthRequired if expired
//   Notes:             OFX is stateless — no server-side cursor. We synthesize incremental
//                      sync by tracking the last-fetched date. Overlap window (re-fetch last
//                      3 days) handles late-posting transactions; dedup in staging handles dupes.
//
// --- Cash App API ---
//   ID():              "cashapp"
//   AuthFlowType():    AuthFlowRedirect (OAuth2)
//   InitiateLink():    Generates OAuth2 authorization URL
//   CompleteLink():    Exchanges auth code for access_token + refresh_token
//   FetchTransactions: GET /payments with pagination cursor
//                      SyncCursor: JSON{"cursor":"...","last_id":"..."}
//   FetchBalances:     GET /balance
//   RefreshLink:       Uses refresh_token to get new access_token
//   Notes:             Cash App has a single "account" per user (no sub-accounts).
//                      ListAccounts returns a single AccountInfo.
//
// --- Open Banking (PSD2/UK) ---
//   ID():              "openbanking-uk" (or "openbanking-eu" for PSD2)
//   AuthFlowType():    AuthFlowRedirect (OAuth2 with consent)
//   InitiateLink():    Registers intent with ASPSP, generates consent URL
//   CompleteLink():    Exchanges auth code, gets consent and access tokens
//   FetchTransactions: GET /accounts/{id}/transactions with date range or continuation
//                      SyncCursor: JSON{"continuation":"...","last_date":"..."}
//   FetchBalances:     GET /accounts/{id}/balances
//   RefreshLink:       OAuth2 refresh_token flow; consent may need re-authorization (90-day limit)
//   Notes:             Regulated API — responses are standardized but banks interpret the
//                      spec differently. Error handling must be more resilient.
//                      90-day re-consent requirement means links go stale periodically.
