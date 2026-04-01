package scrip

// Message payload types for scrip convention messages.
//
// Each type corresponds to a campfire message parseable by CampfireScripStore.applyMessage.
// Fields match the JSON keys the apply* methods expect.
//
// Tag constants are re-exported from the campfire_store.go file (unexported there,
// exported here for use by the exchange engine).

// Scrip convention message tags, matching docs/convention/scrip-operations.md.
const (
	TagScripMint          = "dontguess:scrip-mint"
	TagScripBurn          = "dontguess:scrip-burn"
	TagScripPutPay        = "dontguess:scrip-put-pay"
	TagScripBuyHold       = "dontguess:scrip-buy-hold"
	TagScripSettle        = "dontguess:scrip-settle"
	TagScripAssignPay     = "dontguess:scrip-assign-pay"
	TagScripDisputeRefund = "dontguess:scrip-dispute-refund"
	TagScripLoanMint      = "dontguess:scrip-loan-mint"
	TagScripLoanRepay     = "dontguess:scrip-loan-repay"
	TagScripLoanVigAccrue = "dontguess:scrip-loan-vig-accrue"
)

// MintPayload is the JSON payload for a scrip-mint message.
// State effect: recipient balance += amount; total_supply += amount.
type MintPayload struct {
	Recipient  string `json:"recipient"`
	Amount     int64  `json:"amount"`
	X402TxRef  string `json:"x402_tx_ref"`
	Rate       int64  `json:"rate"`
}

// BurnPayload is the JSON payload for a scrip-burn message.
// State effect: total_burned += amount (balance already removed by prior hold).
type BurnPayload struct {
	Amount    int64  `json:"amount"`
	Reason    string `json:"reason"`    // "matching-fee" | "operator-deflation" | "penalty"
	SourceMsg string `json:"source_msg,omitempty"` // msg ID that triggered this burn
}

// PutPayPayload is the JSON payload for a scrip-put-pay message.
// State effect: seller balance += amount; operator balance -= amount.
type PutPayPayload struct {
	Seller      string `json:"seller"`
	Amount      int64  `json:"amount"`
	TokenCost   int64  `json:"token_cost"`
	DiscountPct int    `json:"discount_pct"`
	ResultHash  string `json:"result_hash"`
	PutMsg      string `json:"put_msg"` // msg ID of the put operation
}

// BuyHoldPayload is the JSON payload for a scrip-buy-hold message.
// State effect: buyer balance -= amount (escrow hold).
type BuyHoldPayload struct {
	Buyer         string `json:"buyer"`
	Amount        int64  `json:"amount"` // price + fee
	Price         int64  `json:"price"`
	Fee           int64  `json:"fee"`
	ReservationID string `json:"reservation_id"`
	BuyMsg        string `json:"buy_msg"`    // msg ID of the buy request
	ExpiresAt     string `json:"expires_at"` // ISO 8601
}

// SettlePayload is the JSON payload for a scrip-settle message.
// State effect: seller balance += residual; operator balance += exchange_revenue;
// total_burned += fee_burned.
type SettlePayload struct {
	ReservationID   string `json:"reservation_id"`
	Seller          string `json:"seller"`
	Residual        int64  `json:"residual"`
	FeeBurned       int64  `json:"fee_burned"`
	ExchangeRevenue int64  `json:"exchange_revenue"`
	MatchMsg        string `json:"match_msg"`   // msg ID of the match operation
	ResultHash      string `json:"result_hash"` // SHA-256 of delivered result
}

// AssignPayPayload is the JSON payload for a scrip-assign-pay message.
// State effect: worker balance += amount; operator balance -= amount.
type AssignPayPayload struct {
	Worker     string `json:"worker"`
	Amount     int64  `json:"amount"`
	TaskType   string `json:"task_type"` // "validate" | "compress" | "freshen"
	AssignMsg  string `json:"assign_msg"`
	ResultHash string `json:"result_hash,omitempty"`
}

// DisputeRefundPayload is the JSON payload for a scrip-dispute-refund message.
// State effect: buyer balance += amount (escrow released).
type DisputeRefundPayload struct {
	Buyer         string `json:"buyer"`
	Amount        int64  `json:"amount"`
	ReservationID string `json:"reservation_id"`
	DisputeMsg    string `json:"dispute_msg"` // msg ID of the dispute resolution
}

// LoanMintPayload is the JSON payload for a scrip-loan-mint message.
// State effect: borrower_balance += principal; total_supply += principal;
// loans[loan_id] = LoanRecord{...}; loansByBorrower[borrower] appends loan_id.
//
// Design ref: docs/design/semantic-matching-marketplace.md §8.2
type LoanMintPayload struct {
	Borrower          string `json:"borrower"`            // agent pubkey receiving minted scrip
	Principal         int64  `json:"principal"`           // loan principal in micro-tokens
	VigRateBPS        int    `json:"vig_rate_bps"`        // vig rate in basis points per hour
	DueAt             string `json:"due_at"`              // ISO 8601 repayment deadline
	LoanID            string `json:"loan_id"`             // unique loan identifier
	SettlementMsgID   string `json:"settlement_msg_id"`   // settlement this loan backs
	CommitmentTokenID string `json:"commitment_token_id"` // commitment token being redeemed
}

// LoanRepayPayload is the JSON payload for a scrip-loan-repay message.
// State effect: LoanRecord.Repaid += amount; totalSupply -= amount;
// totalLoanPrincipal -= amount (when fully repaid); LoanRecord.Status = LoanRepaid
// when Repaid >= Principal.
//
// Design ref: docs/design/semantic-matching-marketplace.md §9
type LoanRepayPayload struct {
	LoanID string `json:"loan_id"` // loan being repaid
	Amount int64  `json:"amount"`  // scrip burned (repayment amount in micro-tokens)
}

// LoanVigAccruePayload is the JSON payload for a scrip-loan-vig-accrue message.
// State effect: LoanRecord.Outstanding += amount. This is a separate accounting
// flow from principal repayment — vig accrues on the original principal and is
// tracked in Outstanding until collected.
//
// Design ref: docs/design/semantic-matching-marketplace.md §9
type LoanVigAccruePayload struct {
	LoanID string `json:"loan_id"` // loan for which vig is accruing
	Amount int64  `json:"amount"`  // vig accrued in this period (micro-tokens)
}
