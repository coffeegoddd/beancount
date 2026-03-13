// Package ledger defines minimal Go interfaces and types for double-entry
// accounting, derived from analysis of beancount's Python core.
//
// Design principles:
//   - Value types where Python uses NamedTuples (structs, not pointers)
//   - Interfaces only where polymorphism is needed (Directive)
//   - shopkeeper's Decimal via shopspring/decimal, not float64
//   - Immutable-by-convention: exported fields, no setters
//   - Account as a typed string, not an object graph
//
// This file is a proposal, not compilable code. It omits imports for brevity.
package ledger

import (
	"time"

	"github.com/shopspring/decimal"
)

// ---------------------------------------------------------------------------
// Primitive types
// ---------------------------------------------------------------------------

// Account is a colon-separated hierarchical name, e.g. "Assets:Bank:Checking".
type Account string

// Currency is an uppercase commodity symbol, e.g. "USD", "HOOL".
type Currency string

// Flag is a single-character transaction/posting status marker.
type Flag rune

// Meta holds arbitrary key-value metadata attached to directives and postings.
// The "filename" and "lineno" keys are always present on directives.
type Meta map[string]any

// ---------------------------------------------------------------------------
// Core value types
// ---------------------------------------------------------------------------

// Amount pairs a decimal number with a currency.
// Corresponds to beancount.core.amount.Amount.
type Amount struct {
	Number   decimal.Decimal
	Currency Currency
}

// Cost is a fully-resolved lot cost (per-unit number, currency, acquisition
// date, optional label). Corresponds to beancount.core.position.Cost.
type Cost struct {
	Number   decimal.Decimal // per-unit cost
	Currency Currency
	Date     time.Time
	Label    string // empty if none
}

// CostSpec is the user-provided, potentially incomplete cost specification
// that the booking algorithm resolves into a Cost.
// Corresponds to beancount.core.position.CostSpec.
type CostSpec struct {
	NumberPer   *decimal.Decimal // nil if unspecified
	NumberTotal *decimal.Decimal // nil if unspecified
	Currency    *Currency        // nil if unspecified
	Date        *time.Time       // nil if unspecified
	Label       *string          // nil if unspecified
	Merge       bool             // true if averaging requested ("*")
}

// Position is a (units, optional cost) pair — the fundamental unit tracked
// in an inventory. Corresponds to beancount.core.position.Position.
type Position struct {
	Units Amount
	Cost  *Cost // nil when not held at cost
}

// ---------------------------------------------------------------------------
// Posting
// ---------------------------------------------------------------------------

// Posting is a single leg of a transaction.
// Corresponds to beancount.core.data.Posting.
type Posting struct {
	Account Account
	Units   *Amount   // nil if to be inferred (auto-balancing)
	Cost    *Cost     // nil if no cost basis; pre-booking may hold CostSpec
	Price   *Amount   // nil if no price conversion
	Flag    *Flag     // nil if no per-posting flag
	Meta    Meta      // nil if no posting-level metadata
}

// ---------------------------------------------------------------------------
// Directives (the Directive interface)
// ---------------------------------------------------------------------------

// Directive is the common interface for all dated ledger entries.
// In beancount these are NamedTuples sharing (meta, date) fields.
type Directive interface {
	// DirectiveMeta returns the metadata dict (always has "filename", "lineno").
	DirectiveMeta() Meta
	// DirectiveDate returns the date of the directive.
	DirectiveDate() time.Time
	// directiveMarker is a sealed-interface method preventing external implementations.
	directiveMarker()
}

// directiveBase provides the shared fields. Embed in every directive struct.
type directiveBase struct {
	Meta Meta
	Date time.Time
}

func (d directiveBase) DirectiveMeta() Meta      { return d.Meta }
func (d directiveBase) DirectiveDate() time.Time  { return d.Date }
func (d directiveBase) directiveMarker()          {}

// Transaction is the primary directive: a balanced set of postings.
type Transaction struct {
	directiveBase
	Flag      Flag
	Payee     string   // empty if absent
	Narration string   // always present
	Tags      []string // without '#' prefix
	Links     []string // without '^' prefix
	Postings  []Posting
}

// Open declares an account, optionally constraining allowed currencies and
// specifying a booking method.
type Open struct {
	directiveBase
	Account    Account
	Currencies []Currency // nil means unrestricted
	Booking    *Booking   // nil means use default
}

// Close marks an account as closed.
type Close struct {
	directiveBase
	Account Account
}

// Balance asserts the expected balance of an account at the start of a date.
type Balance struct {
	directiveBase
	Account    Account
	Amount     Amount
	Tolerance  *decimal.Decimal // nil if not specified
	DiffAmount *Amount          // nil if check passes; set on failure
}

// Pad inserts auto-balancing transactions between two accounts.
type Pad struct {
	directiveBase
	Account       Account
	SourceAccount Account
}

// Commodity is an optional declaration to attach metadata to a currency.
type Commodity struct {
	directiveBase
	Currency Currency
}

// Price declares a market price for a currency pair on a date.
type Price struct {
	directiveBase
	Currency Currency
	Amount   Amount
}

// Note attaches free-text to an account on a date.
type Note struct {
	directiveBase
	Account Account
	Comment string
	Tags    []string
	Links   []string
}

// Event records a named variable's value change over time.
type Event struct {
	directiveBase
	Type        string
	Description string
}

// Document attaches a file path to an account on a date.
type Document struct {
	directiveBase
	Account  Account
	Filename string
	Tags     []string
	Links    []string
}

// Query declares a named BQL query.
type Query struct {
	directiveBase
	Name        string
	QueryString string
}

// Custom is an extension point for user-defined directive types.
type Custom struct {
	directiveBase
	Type   string
	Values []any
}

// ---------------------------------------------------------------------------
// Booking
// ---------------------------------------------------------------------------

// Booking enumerates the lot-matching strategies when reducing inventory.
// Corresponds to beancount.core.data.Booking.
type Booking int

const (
	BookingStrict        Booking = iota // reject ambiguous matches
	BookingStrictWithSize               // strict, but prefer exact size match
	BookingNone                         // allow mixed inventories
	BookingAverage                      // merge lots, average cost
	BookingFIFO                         // first-in first-out
	BookingLIFO                         // last-in first-out
	BookingHIFO                         // highest-cost first-out
)

// ---------------------------------------------------------------------------
// Account classification
// ---------------------------------------------------------------------------

// AccountTypes names the five root account categories.
// Corresponds to beancount.core.account_types.AccountTypes.
type AccountTypes struct {
	Assets      string
	Liabilities string
	Equity      string
	Income      string
	Expenses    string
}

// DefaultAccountTypes is the standard chart-of-accounts root naming.
var DefaultAccountTypes = AccountTypes{
	Assets:      "Assets",
	Liabilities: "Liabilities",
	Equity:      "Equity",
	Income:      "Income",
	Expenses:    "Expenses",
}

// ---------------------------------------------------------------------------
// Inventory
// ---------------------------------------------------------------------------

// InventoryKey uniquely identifies a position within an inventory.
// Corresponds to beancount's (currency, cost) tuple key.
type InventoryKey struct {
	Currency Currency
	Cost     *Cost // nil for positions without cost basis
}

// Inventory tracks a set of positions keyed by (currency, cost).
// Corresponds to beancount.core.inventory.Inventory.
//
// The minimal interface:
type Inventory interface {
	// AddAmount adds units with an optional cost, returning the prior position
	// (if any) and how the lot was matched.
	AddAmount(units Amount, cost *Cost) (prior *Position, result MatchResult)

	// AddPosition adds a Position to the inventory.
	AddPosition(pos Position) (prior *Position, result MatchResult)

	// Positions returns all current positions (no guaranteed order).
	Positions() []Position

	// IsEmpty returns true if the inventory holds no positions.
	IsEmpty() bool

	// CurrencyUnits returns the total units for the given currency,
	// summing across all lots.
	CurrencyUnits(currency Currency) Amount

	// Reduce applies a conversion function to each position, returning a
	// new inventory of the results. Used with GetUnits, GetCost, GetValue.
	Reduce(fn func(Position) Amount) Inventory
}

// MatchResult indicates how a lot addition was matched.
type MatchResult int

const (
	MatchCreated  MatchResult = iota // new lot
	MatchReduced                     // existing lot reduced
	MatchAugmented                   // existing lot augmented
	MatchIgnored                     // zero amount, no change
)

// ---------------------------------------------------------------------------
// Conversion functions (beancount.core.convert)
// ---------------------------------------------------------------------------

// GetUnits returns the units of a position (identity).
// func GetUnits(pos Position) Amount

// GetCost returns the total cost basis: units.number * cost.number.
// Falls back to units if no cost.
// func GetCost(pos Position) Amount

// GetWeight returns the "balance weight" — cost if available, else price, else units.
// This is the amount used to verify transaction balance.
// func GetWeight(posting Posting) Amount

// GetValue returns market value using a price map.
// func GetValue(pos Position, pm PriceMap, date time.Time) Amount

// ---------------------------------------------------------------------------
// Price database (beancount.core.prices)
// ---------------------------------------------------------------------------

// PriceMap maps (base, quote) currency pairs to time-sorted price histories.
// Includes both forward and inverse pairs for symmetric lookup.
type PriceMap interface {
	// GetPrice returns the price for (base, quote) at or before date.
	// Returns zero time and nil decimal if not found.
	GetPrice(base, quote Currency, date time.Time) (priceDate time.Time, rate *decimal.Decimal)
}

// ---------------------------------------------------------------------------
// Realization (beancount.core.realization)
// ---------------------------------------------------------------------------

// RealAccount is a tree node representing a realized account with its
// transaction postings and running balance.
type RealAccount struct {
	Account     Account
	TxnPostings []TxnPosting // postings touching this account (not children)
	Balance     Inventory    // running balance
	Children    map[string]*RealAccount
}

// TxnPosting pairs a posting with its parent transaction.
type TxnPosting struct {
	Txn     *Transaction
	Posting *Posting
}

// ---------------------------------------------------------------------------
// Processing pipeline (beancount.loader + beancount.parser.booking)
// ---------------------------------------------------------------------------

// The beancount processing pipeline in order:
//
//  1. Parse: text -> []Directive (with incomplete amounts, CostSpec not Cost)
//  2. Book:  resolve CostSpec -> Cost, interpolate missing amounts,
//            apply booking method (FIFO/LIFO/etc) per account
//  3. Plugins (pre): document discovery
//  4. Plugins (auto): user-specified plugins from "plugin" directives
//  5. Plugins (post): pad, balance checking
//  6. Validate: structural integrity checks
//
// A Plugin transforms directives:
//
//   type Plugin func(entries []Directive, options Options) ([]Directive, []error)

// Loader is the top-level entry point.
type Loader interface {
	// LoadFile parses, books, runs plugins, and validates a beancount file.
	LoadFile(filename string) (directives []Directive, errors []error, options Options)
}

// Options holds parsed option values from the input file.
// Corresponds to beancount's options_map dict.
type Options map[string]any

// ---------------------------------------------------------------------------
// Sorting (beancount.core.data.entry_sortkey)
// ---------------------------------------------------------------------------

// Directive sort order on the same date:
//   Open < Balance < (everything else) < Document < Close
// Secondary key: line number in source file.
//
// The SortDirectives function should sort a []Directive slice in this order.

// ---------------------------------------------------------------------------
// Summary of type mapping: Python -> Go
// ---------------------------------------------------------------------------
//
//  Python                          Go
//  ─────────────────────────────── ────────────────────────────────
//  str (Account)                   Account (type Account string)
//  str (Currency)                  Currency (type Currency string)
//  str (Flag)                      Flag (type Flag rune)
//  dict[str,Any] (Meta)            Meta (map[string]any)
//  Decimal                         decimal.Decimal (shopspring)
//  datetime.date                   time.Time (date portion only)
//  Amount (NamedTuple)             Amount struct
//  Cost (NamedTuple)               Cost struct
//  CostSpec (NamedTuple)           CostSpec struct (pointer fields for optionality)
//  Position (NamedTuple)           Position struct
//  Posting (NamedTuple)            Posting struct
//  Transaction (NamedTuple)        Transaction struct (embeds directiveBase)
//  Open, Close, ... (NamedTuples)  Concrete structs embedding directiveBase
//  Directive (Union type)          Directive interface (sealed)
//  Booking (Enum)                  Booking int + iota constants
//  AccountTypes (NamedTuple)       AccountTypes struct
//  Inventory (dict subclass)       Inventory interface
//  MatchResult (Enum)              MatchResult int + iota constants
//  PriceMap (dict subclass)        PriceMap interface
//  RealAccount (dict subclass)     RealAccount struct (explicit children map)
//  Plugin (callable)               Plugin func type
//  Options (dict)                  Options map[string]any
