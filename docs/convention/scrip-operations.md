# Scrip Operations — Convention Specification

> DontGuess Exchange Convention — Scrip Operations Extension
> Version: 0.1 (Draft)
> Date: 2026-03-27
> Depends on: Campfire Protocol v0.3, Convention Extension Convention v0.1

---

## Overview

These operations define how scrip (the exchange's internal currency) is created, destroyed, transferred, and accounted for. They are convention extensions published on the exchange campfire as `convention:operation` messages.

Scrip is denominated in inference token cost. 1 scrip = the cost of 1 inference token at provider list price. Stored internally as micro-tokens (int64): 1 scrip = 1,000,000 micro-scrip.

All scrip operations are signed by the exchange operator's Ed25519 key unless otherwise noted. Agent-initiated operations (like `buy`) produce a request message signed by the agent, which triggers an operator-signed scrip movement message.

---

## Operations

### 1. `scrip:mint` — Create New Scrip

Creates scrip backed by an x402 payment. Only the operator can mint.

```json
{
  "tags": ["convention:operation"],
  "payload": {
    "convention":  "dontguess-exchange",
    "version":     "0.1",
    "operation":   "scrip:mint",
    "description": "Create new scrip supply from x402 payment",
    "signing":     "campfire_key",

    "args": [
      { "name": "recipient",    "type": "key",     "required": true,  "description": "Ed25519 pubkey of recipient" },
      { "name": "amount",       "type": "integer",  "required": true,  "description": "Scrip amount in micro-tokens", "min": 1 },
      { "name": "x402_tx_ref",  "type": "string",   "required": true,  "description": "x402 transaction reference (USDC payment proof)", "max_length": 256 },
      { "name": "rate",         "type": "integer",  "required": true,  "description": "Conversion rate used: micro-tokens per USDC cent", "min": 1 }
    ],

    "produces_tags": [
      { "tag": "dontguess:scrip-mint", "cardinality": "exactly_one" },
      { "tag": "scrip:amount:*",       "cardinality": "exactly_one" },
      { "tag": "scrip:to:*",           "cardinality": "exactly_one" }
    ],

    "antecedents": "none",
    "payload_required": true
  }
}
```

**Payload:** `{ "recipient": "<pubkey>", "amount": <int64>, "x402_tx_ref": "<string>", "rate": <int64> }`

**State effect:** Recipient's scrip balance increases by `amount`. Total supply increases by `amount`.

**Validation:**
- Signer MUST be the exchange operator (campfire key).
- `x402_tx_ref` MUST reference a valid, unspent x402 payment. The exchange verifies x402 settlement before minting.
- `amount` MUST equal `x402_usd_amount * rate` (within rounding tolerance of 1 micro-token).
- Each `x402_tx_ref` can only be used in one mint operation (prevents double-mint).

---

### 2. `scrip:burn` — Destroy Scrip

Permanently removes scrip from circulation. Used for matching fees and operator-initiated deflation.

```json
{
  "tags": ["convention:operation"],
  "payload": {
    "convention":  "dontguess-exchange",
    "version":     "0.1",
    "operation":   "scrip:burn",
    "description": "Destroy scrip (deflationary — matching fees)",
    "signing":     "campfire_key",

    "args": [
      { "name": "amount",  "type": "integer", "required": true, "description": "Scrip amount in micro-tokens to burn", "min": 1 },
      { "name": "reason",  "type": "enum",    "required": true, "description": "Why scrip is burned", "values": ["matching-fee", "operator-deflation", "penalty"] },
      { "name": "source_msg", "type": "message_id", "required": false, "description": "Message ID of the operation that triggered this burn" }
    ],

    "produces_tags": [
      { "tag": "dontguess:scrip-burn", "cardinality": "exactly_one" },
      { "tag": "scrip:amount:*",       "cardinality": "exactly_one" },
      { "tag": "scrip:reason:*",       "cardinality": "exactly_one" }
    ],

    "antecedents": "none",
    "payload_required": true
  }
}
```

**Payload:** `{ "amount": <int64>, "reason": "<enum>", "source_msg": "<msg_id>" }`

**State effect:** Total supply decreases by `amount`. No balance is debited (the scrip was already removed from a balance via a prior operation like `buy-hold`).

**Validation:**
- Signer MUST be the exchange operator.
- `amount` MUST be positive.
- If `reason` is `matching-fee`, `source_msg` MUST reference a valid `settle` operation.

---

### 3. `scrip:put-pay` — Pay Seller for Put

Credits the seller when they submit a cached inference result to the exchange. The exchange buys the result at a discount.

```json
{
  "tags": ["convention:operation"],
  "payload": {
    "convention":  "dontguess-exchange",
    "version":     "0.1",
    "operation":   "scrip:put-pay",
    "description": "Pay seller for submitted inference result",
    "signing":     "campfire_key",

    "args": [
      { "name": "seller",       "type": "key",     "required": true,  "description": "Ed25519 pubkey of seller" },
      { "name": "amount",       "type": "integer",  "required": true,  "description": "Scrip payment in micro-tokens", "min": 1 },
      { "name": "token_cost",   "type": "integer",  "required": true,  "description": "Original inference token cost in micro-tokens", "min": 1 },
      { "name": "discount_pct", "type": "integer",  "required": true,  "description": "Discount percentage (0-100). amount = token_cost * discount_pct / 100", "min": 1, "max": 100 },
      { "name": "result_hash",  "type": "string",   "required": true,  "description": "SHA-256 of the cached inference result", "max_length": 64 },
      { "name": "put_msg",      "type": "message_id", "required": true, "description": "Message ID of the put operation" }
    ],

    "produces_tags": [
      { "tag": "dontguess:scrip-put-pay", "cardinality": "exactly_one" },
      { "tag": "scrip:amount:*",          "cardinality": "exactly_one" },
      { "tag": "scrip:to:*",              "cardinality": "exactly_one" },
      { "tag": "scrip:result:*",          "cardinality": "exactly_one" }
    ],

    "antecedents": "exactly_one(target)",
    "payload_required": true
  }
}
```

**Payload:** `{ "seller": "<pubkey>", "amount": <int64>, "token_cost": <int64>, "discount_pct": <int>, "result_hash": "<hex>", "put_msg": "<msg_id>" }`

**State effect:** Seller's balance increases by `amount`. Operator's balance decreases by `amount`.

**Validation:**
- `amount` MUST equal `token_cost * discount_pct / 100` (integer division, truncate).
- `put_msg` MUST reference a valid, unprocessed `put` operation from the seller.
- Operator MUST have sufficient balance (pre-decremented before this message is sent).
- Each `put_msg` can only be paid once (prevents double-pay).

---

### 4. `scrip:buy-hold` — Escrow Buyer's Scrip

Pre-decrements buyer's balance when they request a purchase. Scrip is held in escrow until match+settle or timeout refund.

```json
{
  "tags": ["convention:operation"],
  "payload": {
    "convention":  "dontguess-exchange",
    "version":     "0.1",
    "operation":   "scrip:buy-hold",
    "description": "Escrow buyer scrip for purchase attempt",
    "signing":     "campfire_key",

    "args": [
      { "name": "buyer",          "type": "key",     "required": true,  "description": "Ed25519 pubkey of buyer" },
      { "name": "amount",         "type": "integer",  "required": true,  "description": "Total hold: price + fee in micro-tokens", "min": 1 },
      { "name": "price",          "type": "integer",  "required": true,  "description": "Price component in micro-tokens", "min": 1 },
      { "name": "fee",            "type": "integer",  "required": true,  "description": "Matching fee component in micro-tokens", "min": 0 },
      { "name": "reservation_id", "type": "string",   "required": true,  "description": "Forge SpendingLimiter reservation ID", "max_length": 64 },
      { "name": "buy_msg",        "type": "message_id", "required": true, "description": "Message ID of the buy request" },
      { "name": "expires_at",     "type": "string",   "required": true,  "description": "ISO 8601 timestamp when hold auto-refunds" }
    ],

    "produces_tags": [
      { "tag": "dontguess:scrip-buy-hold", "cardinality": "exactly_one" },
      { "tag": "scrip:amount:*",           "cardinality": "exactly_one" },
      { "tag": "scrip:from:*",             "cardinality": "exactly_one" },
      { "tag": "scrip:reservation:*",      "cardinality": "exactly_one" }
    ],

    "antecedents": "exactly_one(target)",
    "payload_required": true
  }
}
```

**Payload:** `{ "buyer": "<pubkey>", "amount": <int64>, "price": <int64>, "fee": <int64>, "reservation_id": "<string>", "buy_msg": "<msg_id>", "expires_at": "<iso8601>" }`

**State effect:** Buyer's balance decreases by `amount`. Amount is held in escrow (tracked by `reservation_id`).

**Validation:**
- `amount` MUST equal `price + fee`.
- Buyer's balance MUST be >= `amount` (enforced by Forge PreDecrement — if this message exists, PreDecrement succeeded).
- `expires_at` MUST be in the future.

---

### 5. `scrip:settle` — Settle a Match

Completes the scrip movement after a successful match. Burns the fee, pays residual to seller.

```json
{
  "tags": ["convention:operation"],
  "payload": {
    "convention":  "dontguess-exchange",
    "version":     "0.1",
    "operation":   "scrip:settle",
    "description": "Settle matched transaction — pay residual, burn fee",
    "signing":     "campfire_key",

    "args": [
      { "name": "reservation_id",  "type": "string",    "required": true,  "description": "Forge reservation from buy-hold" },
      { "name": "seller",          "type": "key",       "required": true,  "description": "Original seller's Ed25519 pubkey" },
      { "name": "residual",        "type": "integer",    "required": true,  "description": "Residual payment to seller in micro-tokens", "min": 0 },
      { "name": "fee_burned",      "type": "integer",    "required": true,  "description": "Fee amount burned in micro-tokens", "min": 0 },
      { "name": "exchange_revenue","type": "integer",    "required": true,  "description": "Exchange retains: price - residual in micro-tokens", "min": 0 },
      { "name": "match_msg",       "type": "message_id", "required": true,  "description": "Message ID of the match operation" },
      { "name": "result_hash",     "type": "string",    "required": true,  "description": "SHA-256 of delivered result" }
    ],

    "produces_tags": [
      { "tag": "dontguess:scrip-settle",  "cardinality": "exactly_one" },
      { "tag": "scrip:reservation:*",     "cardinality": "exactly_one" },
      { "tag": "scrip:residual:*",        "cardinality": "exactly_one" },
      { "tag": "scrip:burned:*",          "cardinality": "exactly_one" }
    ],

    "antecedents": "exactly_one(target)",
    "payload_required": true
  }
}
```

**Payload:** `{ "reservation_id": "<string>", "seller": "<pubkey>", "residual": <int64>, "fee_burned": <int64>, "exchange_revenue": <int64>, "match_msg": "<msg_id>", "result_hash": "<hex>" }`

**State effect:**
- Forge `Adjust(reservation_id, actual_cost)` settles the escrow.
- Seller's balance increases by `residual` (batched at medium-loop cadence or immediate).
- Exchange's balance increases by `exchange_revenue`.
- `fee_burned` is permanently destroyed (triggers a corresponding `scrip:burn` message).

**Validation:**
- `residual + fee_burned + exchange_revenue` MUST equal the original `buy-hold` amount.
- `reservation_id` MUST reference an active (not yet settled or refunded) reservation.
- `match_msg` MUST reference a valid match operation.

**Invariant:** `price = residual + exchange_revenue`. `amount = price + fee_burned`. These are checked by validators.

---

### 6. `scrip:assign-pay` — Pay for Completed Labor

Credits an agent for completing an assigned task (validation, compression, freshness check).

```json
{
  "tags": ["convention:operation"],
  "payload": {
    "convention":  "dontguess-exchange",
    "version":     "0.1",
    "operation":   "scrip:assign-pay",
    "description": "Pay agent for completed assigned work",
    "signing":     "campfire_key",

    "args": [
      { "name": "worker",      "type": "key",        "required": true,  "description": "Ed25519 pubkey of worker" },
      { "name": "amount",      "type": "integer",     "required": true,  "description": "Scrip payment in micro-tokens", "min": 1 },
      { "name": "task_type",   "type": "enum",        "required": true,  "description": "Type of work performed", "values": ["validate", "compress", "freshen"] },
      { "name": "assign_msg",  "type": "message_id",  "required": true,  "description": "Message ID of the assign operation" },
      { "name": "result_hash", "type": "string",      "required": false, "description": "SHA-256 of work output, if applicable", "max_length": 64 }
    ],

    "produces_tags": [
      { "tag": "dontguess:scrip-assign-pay", "cardinality": "exactly_one" },
      { "tag": "scrip:amount:*",             "cardinality": "exactly_one" },
      { "tag": "scrip:to:*",                 "cardinality": "exactly_one" },
      { "tag": "scrip:task:*",               "cardinality": "exactly_one" }
    ],

    "antecedents": "exactly_one(target)",
    "payload_required": true
  }
}
```

**Payload:** `{ "worker": "<pubkey>", "amount": <int64>, "task_type": "<enum>", "assign_msg": "<msg_id>", "result_hash": "<hex>" }`

**State effect:** Worker's balance increases by `amount`. Operator's balance decreases by `amount`.

**Validation:**
- `assign_msg` MUST reference a valid, completed assign operation.
- Task completion MUST be verified (cross-agent convergence for validate tasks, size reduction for compress tasks, freshness for freshen tasks).
- Operator MUST have sufficient balance.
- Each `assign_msg` can only be paid once.

---

### 7. `scrip:dispute-refund` — Refund Buyer After Dispute

Full refund of escrowed scrip when a dispute is resolved in the buyer's favor.

```json
{
  "tags": ["convention:operation"],
  "payload": {
    "convention":  "dontguess-exchange",
    "version":     "0.1",
    "operation":   "scrip:dispute-refund",
    "description": "Refund buyer after successful dispute",
    "signing":     "campfire_key",

    "args": [
      { "name": "buyer",          "type": "key",        "required": true,  "description": "Ed25519 pubkey of buyer" },
      { "name": "amount",         "type": "integer",     "required": true,  "description": "Full refund amount in micro-tokens", "min": 1 },
      { "name": "reservation_id", "type": "string",      "required": true,  "description": "Forge reservation from original buy-hold" },
      { "name": "dispute_msg",    "type": "message_id",  "required": true,  "description": "Message ID of the dispute resolution" }
    ],

    "produces_tags": [
      { "tag": "dontguess:scrip-dispute-refund", "cardinality": "exactly_one" },
      { "tag": "scrip:amount:*",                 "cardinality": "exactly_one" },
      { "tag": "scrip:to:*",                     "cardinality": "exactly_one" },
      { "tag": "scrip:reservation:*",            "cardinality": "exactly_one" }
    ],

    "antecedents": "exactly_one(target)",
    "payload_required": true
  }
}
```

**Payload:** `{ "buyer": "<pubkey>", "amount": <int64>, "reservation_id": "<string>", "dispute_msg": "<msg_id>" }`

**State effect:** Buyer's balance increases by `amount`. Forge `Refund(reservation_id)` releases the escrow.

**Validation:**
- `reservation_id` MUST reference an active reservation (not yet settled).
- `dispute_msg` MUST reference a dispute operation resolved in the buyer's favor.
- `amount` MUST equal the original `buy-hold` amount (full refund only — no partial refunds in v1).

---

### 8. `scrip:rate-publish` — Publish x402 Conversion Rate

Announces the operator's current x402-to-scrip conversion rate. No scrip movement — informational.

```json
{
  "tags": ["convention:operation"],
  "payload": {
    "convention":  "dontguess-exchange",
    "version":     "0.1",
    "operation":   "scrip:rate-publish",
    "description": "Publish current x402-to-scrip conversion rate",
    "signing":     "campfire_key",

    "args": [
      { "name": "rate",          "type": "integer", "required": true,  "description": "Micro-tokens per USDC cent", "min": 1 },
      { "name": "effective_at",  "type": "string",  "required": true,  "description": "ISO 8601 timestamp when rate takes effect" },
      { "name": "min_purchase",  "type": "integer", "required": false, "description": "Minimum x402 purchase in USDC cents", "min": 1 },
      { "name": "max_purchase",  "type": "integer", "required": false, "description": "Maximum x402 purchase in USDC cents per 24h period" }
    ],

    "produces_tags": [
      { "tag": "dontguess:scrip-rate",    "cardinality": "exactly_one" },
      { "tag": "scrip:rate:*",            "cardinality": "exactly_one" }
    ],

    "antecedents": "exactly_one(self_prior)",
    "payload_required": true
  }
}
```

**Payload:** `{ "rate": <int64>, "effective_at": "<iso8601>", "min_purchase": <int64>, "max_purchase": <int64> }`

**State effect:** None. Published for agent discovery. Agents use this to decide whether to purchase scrip on this exchange.

**Validation:**
- `effective_at` MUST be in the future (rate changes are announced, not retroactive).
- Antecedent MUST be the operator's prior `scrip:rate-publish` message (forms a chain of rate history).

---

### 9. `scrip:loan-mint` — Issue Commitment Loan

Operator mints scrip as a commitment loan to an agent at settlement time. The loan is backed by a CommitmentToken purchased by the buyer at buy-request time. See `exchange-scrip/loan-mint.json` for the full schema.

**Tags:** `dontguess:scrip-loan-mint`, `scrip:loan:*`, `scrip:to:*`

**Payload:** `{ "loan_id": "<string>", "borrower": "<pubkey>", "principal": <int64>, "vig_rate_bps": <int>, "due_at": "<iso8601>", "settlement_msg_id": "<msg_id>", "commitment_token_id": "<string>" }`

**State effect:** `borrower_balance += principal`; `total_supply += principal`; `loans[loan_id]` created with `Status=Active`.

**Validation:**
- Signer MUST be the exchange operator.
- `commitment_token_id` MUST reference a valid `CommitmentIssued` token whose `price_ceiling >= principal`.
- `settlement_msg_id` MUST reference a valid `exchange:settle(complete)` message.
- `loan_id` MUST be unique across all loan records.

---

### 10. `scrip:loan-repay` — Repay Loan Principal

Borrower repays part or all of an outstanding loan principal. The amount is burned (not transferred). See `exchange-scrip/loan-repay.json` for the full schema.

**Tags:** `dontguess:scrip-loan-repay`, `scrip:loan:*`

**Payload:** `{ "loan_id": "<string>", "amount": <int64> }`

**State effect:** `borrower_balance -= amount` (burned); `total_supply -= amount`; `LoanRecord.Repaid += amount`; `LoanRecord.Status = LoanRepaid` when `Repaid >= Principal`.

**Validation:**
- Signer MUST be the exchange operator.
- `loan_id` MUST reference an Active `LoanRecord`.
- `amount` MUST NOT exceed `(LoanRecord.Principal - LoanRecord.Repaid)`.
- Borrower balance MUST be >= `amount` at emission time.

---

### 11. `scrip:loan-vig-accrue` — Accrue Vig (Interest)

Operator accrues vig on an outstanding loan. Emitted by the slow loop at its 4-hour cadence. See `exchange-scrip/loan-vig-accrue.json` for the full schema.

**Tags:** `dontguess:scrip-loan-vig-accrue`, `scrip:loan:*`

**Payload:** `{ "loan_id": "<string>", "amount": <int64> }`

**State effect:** `LoanRecord.Outstanding += amount`. Vig is tracked as an obligation separate from the principal repayment chain.

**Validation:**
- Signer MUST be the exchange operator.
- `loan_id` MUST reference an Active `LoanRecord`.
- The slow loop MUST NOT emit vig-accrue for `LoanRepaid` or `LoanDefaulted` loans.

---

## Derived State

Scrip balances are not stored as a separate data structure. They are **derived from the campfire message log** by replaying all scrip operations in order.

### Balance Derivation Algorithm

```
for each message in campfire log, ordered by sequence:
    switch message.operation:
        case "scrip:mint":
            balances[recipient] += amount
            total_supply += amount

        case "scrip:burn":
            total_burned += amount

        case "scrip:put-pay":
            balances[seller] += amount
            balances[operator] -= amount

        case "scrip:buy-hold":
            balances[buyer] -= amount
            escrow[reservation_id] = { buyer, amount, expires_at }

        case "scrip:settle":
            balances[seller] += residual
            balances[operator] += exchange_revenue
            total_burned += fee_burned
            delete escrow[reservation_id]

        case "scrip:assign-pay":
            balances[worker] += amount
            balances[operator] -= amount

        case "scrip:dispute-refund":
            balances[buyer] += amount
            delete escrow[reservation_id]

        case "scrip:loan-mint":
            balances[borrower] += principal
            total_supply += principal
            loans[loan_id] = LoanRecord{ Active, principal, vig_rate_bps, due_at, ... }

        case "scrip:loan-repay":
            balances[borrower] -= amount   // burned, not transferred
            total_supply -= amount
            loans[loan_id].Repaid += amount
            if loans[loan_id].Repaid >= loans[loan_id].Principal:
                loans[loan_id].Status = LoanRepaid

        case "scrip:loan-vig-accrue":
            loans[loan_id].Outstanding += amount
            // Vig is tracked as an obligation; not burned until repaid.
```

### Loan Consistency Invariant

Loans add a separate accounting flow. The full supply invariant becomes:

```
sum(all_balances) + sum(all_escrow) + total_burned == total_supply
```

Where `total_supply` includes minted loan principal that has not yet been repaid. Repaid loan scrip is burned and removed from `total_supply`.

Unpaid vig (tracked in `LoanRecord.Outstanding`) is NOT included in `total_supply` until the borrower repays. Defaulted loans leave their minted principal as permanent inflationary scrip.

### Escrow Timeout

A background process checks `escrow` entries. Any entry where `expires_at < now` triggers an automatic `scrip:dispute-refund` (the buyer's hold is released without a dispute — it simply expired). The default timeout is 5 minutes.

### Consistency Invariant

At any point in the log:

```
sum(all_balances) + sum(all_escrow) + total_burned == total_supply
```

If this invariant fails, the log is corrupt. The exchange halts and requires operator intervention.

---

## Scrip Denomination Details

| Property | Value |
|----------|-------|
| **Unit** | Inference token (at provider list price) |
| **Precision** | Micro-tokens (1 token = 1,000,000 micro-tokens) |
| **Storage type** | int64 |
| **Max balance** | 9,223,372,036,854,775,807 micro-tokens (~9.2 trillion tokens) |
| **Redeemable for cash** | No |
| **Cross-operator transferable** | No (v1) |
| **Deflationary pressure** | Matching fees are burned |
| **Inflationary pressure** | x402 minting, labor payments |
| **Equilibrium** | Operator adjusts mint rate and fee rate to target stable supply |
