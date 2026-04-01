package scrip

import "time"

// CommitmentToken is a pre-committed price ceiling issued at buy-request time.
// Scrip is minted at settlement when work is delivered (NOT at request time).
// Its lifecycle is bound to the originating ActiveOrder — on order expiry the
// token expires with it.
//
// Design ref: docs/design/semantic-matching-marketplace.md §3.3
type CommitmentToken struct {
	TokenID       string
	PriceCeiling  int64            // max price buyer will accept via loan
	VigRateBPS    int              // e.g. 200 = 2% per hour
	ExpiresAt     time.Time        // must not exceed order expiry
	CommitmentFee int64            // option premium, paid upfront, non-refundable
	Status        CommitmentStatus // Issued, Redeemed, Expired
}

// CommitmentStatus is the lifecycle state of a CommitmentToken.
type CommitmentStatus int

const (
	CommitmentIssued   CommitmentStatus = iota // token active, awaiting settlement
	CommitmentRedeemed                         // token consumed at settlement
	CommitmentExpired                          // token lapsed (order expired or price exceeded ceiling)
)

// LoanRecord is the in-memory record of a minted scrip loan.
// Created by applyLoanMint; updated by loan-repay and loan-vig-accrue (Wave 3b).
//
// Design ref: docs/design/semantic-matching-marketplace.md §9
type LoanRecord struct {
	LoanID          string
	BorrowerKey     string
	Principal       int64      // loan principal in micro-tokens
	VigRateBPS      int        // basis points per hour
	DueAt           time.Time
	Repaid          int64      // total repaid so far
	Outstanding     int64      // accrued vig not yet collected (Wave 3b)
	SettlementMsgID string     // the settlement this loan backs
	CommitmentID    string     // commitment token that authorized this loan
	Status          LoanStatus // Active, Repaid, Defaulted
}

// LoanStatus is the lifecycle state of a LoanRecord.
type LoanStatus int

const (
	LoanActive    LoanStatus = iota // loan outstanding, repayment pending
	LoanRepaid                      // loan fully repaid (principal + vig)
	LoanDefaulted                   // borrower failed to repay; permanent inflationary record
)
