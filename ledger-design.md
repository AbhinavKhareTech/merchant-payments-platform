# Ledger Design

## Principles

The ledger is the system of record for all financial state. Every movement of money, whether real (settlement) or virtual (authorization hold), is recorded as a balanced journal entry.

Three non-negotiable invariants:

1. **Conservation:** The sum of all debits equals the sum of all credits, always
2. **Immutability:** Entries are append-only. Corrections are recorded as new reversing entries, never as mutations
3. **Auditability:** Every entry traces back to the event that caused it, the service that produced it, and the timestamp at which it was recorded

## Data Model

### Accounts

Every entity that can hold or owe money has an account:

```sql
CREATE TABLE accounts (
    account_id      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_type    VARCHAR(50) NOT NULL,    -- ASSET, LIABILITY, REVENUE, EXPENSE
    entity_type     VARCHAR(50) NOT NULL,    -- MERCHANT, CARDHOLDER, PLATFORM, NETWORK
    entity_id       VARCHAR(100) NOT NULL,
    currency        CHAR(3) NOT NULL,        -- ISO 4217
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (entity_type, entity_id, currency, account_type)
);
```

Account types follow standard double-entry conventions:
- **ASSET:** Money the platform holds or is owed (merchant receivables, bank deposits)
- **LIABILITY:** Money the platform owes (merchant payables, cardholder refunds pending)
- **REVENUE:** Platform earnings (processing fees, interchange share)
- **EXPENSE:** Platform costs (network fees, chargebacks absorbed)

### Journal Entries

```sql
CREATE TABLE journal_entries (
    entry_id        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    transaction_id  UUID NOT NULL,
    entry_type      VARCHAR(50) NOT NULL,    -- AUTH_HOLD, CAPTURE, REFUND, FEE, SETTLEMENT, CHARGEBACK
    description     TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by      VARCHAR(100) NOT NULL,   -- service that created this entry
    event_id        UUID NOT NULL,           -- source event that triggered this entry
    idempotency_key VARCHAR(255) NOT NULL UNIQUE
);

CREATE TABLE journal_lines (
    line_id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    entry_id        UUID NOT NULL REFERENCES journal_entries(entry_id),
    account_id      UUID NOT NULL REFERENCES accounts(account_id),
    direction       VARCHAR(6) NOT NULL CHECK (direction IN ('DEBIT', 'CREDIT')),
    amount          BIGINT NOT NULL CHECK (amount > 0),  -- in minor units (cents)
    currency        CHAR(3) NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

### Balance Computation

Balances are computed in two ways:

**Real-time (on-demand):** Sum of all journal lines for an account. Used for authorization checks and merchant dashboards.

```sql
SELECT
    a.account_id,
    a.currency,
    COALESCE(SUM(CASE WHEN jl.direction = 'DEBIT' THEN jl.amount ELSE 0 END), 0) AS total_debits,
    COALESCE(SUM(CASE WHEN jl.direction = 'CREDIT' THEN jl.amount ELSE 0 END), 0) AS total_credits
FROM accounts a
LEFT JOIN journal_lines jl ON a.account_id = jl.account_id
WHERE a.account_id = $1
GROUP BY a.account_id, a.currency;
```

**Materialized (snapshotted):** Periodically computed balance snapshots stored in a separate table. Used for settlement computation, reporting, and reconciliation. Snapshots are taken every 5 minutes and at end-of-day.

```sql
CREATE TABLE balance_snapshots (
    snapshot_id     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id      UUID NOT NULL REFERENCES accounts(account_id),
    balance         BIGINT NOT NULL,
    currency        CHAR(3) NOT NULL,
    snapshot_at     TIMESTAMPTZ NOT NULL,
    last_entry_id   UUID NOT NULL           -- watermark for incremental computation
);
```

## Transaction Lifecycle in the Ledger

### Authorization

```
DEBIT   cardholder.available_balance     $100.00
CREDIT  merchant.pending_auth            $100.00
```

This creates a "hold" on the cardholder's available balance. The merchant sees a pending authorization but cannot access the funds yet.

### Capture (Full)

```
DEBIT   merchant.pending_auth            $100.00
CREDIT  merchant.captured_balance         $97.10
CREDIT  platform.interchange_receivable    $1.80
CREDIT  platform.scheme_fee_payable        $0.30
CREDIT  platform.processing_revenue        $0.80
```

The pending authorization is released and replaced by the final captured amount, net of fees.

### Capture (Partial: $60 of $100 authorized)

```
-- Capture $60
DEBIT   merchant.pending_auth             $60.00
CREDIT  merchant.captured_balance          $58.26
CREDIT  platform.interchange_receivable     $1.08
CREDIT  platform.scheme_fee_payable         $0.18
CREDIT  platform.processing_revenue         $0.48

-- Release remaining hold ($40)
DEBIT   merchant.pending_auth              $40.00
CREDIT  cardholder.available_balance       $40.00
```

### Void (Authorization Reversal)

```
DEBIT   merchant.pending_auth            $100.00
CREDIT  cardholder.available_balance     $100.00
```

The hold is fully released. No fees are charged.

### Refund

```
DEBIT   merchant.captured_balance         $97.10
DEBIT   platform.processing_revenue        $0.80
DEBIT   platform.interchange_receivable    $1.80
DEBIT   platform.scheme_fee_payable        $0.30
CREDIT  cardholder.available_balance     $100.00
```

### Chargeback

```
-- Initial chargeback
DEBIT   merchant.captured_balance         $97.10
DEBIT   platform.processing_revenue        $0.80
CREDIT  cardholder.available_balance     $100.00
CREDIT  platform.scheme_fee_payable        $0.30    (scheme fee reversed)
DEBIT   platform.interchange_receivable    $1.80    (interchange clawed back)
DEBIT   merchant.chargeback_fee_payable   $25.00    (chargeback penalty)
CREDIT  platform.chargeback_fee_revenue   $25.00
```

### Settlement Payout

```
DEBIT   merchant.captured_balance       $5,000.00
CREDIT  merchant.bank_transfer_pending  $5,000.00

-- After bank confirms transfer
DEBIT   merchant.bank_transfer_pending  $5,000.00
CREDIT  merchant.settled                $5,000.00
```

## Consistency Guarantees

### Write Path

All ledger writes use PostgreSQL's SERIALIZABLE isolation level. This prevents:
- **Lost updates:** Two concurrent captures of the same authorization
- **Phantom reads:** Balance checks that miss concurrent writes
- **Write skew:** Authorization based on a stale balance

The cost of serializable isolation is increased serialization failures (aborted transactions). The ledger service handles this with retry logic (up to 3 retries with exponential backoff).

### Reconciliation

The reconciliation engine runs continuously and performs three checks:

1. **Balance Invariant:** For every journal entry, sum(debits) = sum(credits). Violations trigger a P0 alert.
2. **Event Reconciliation:** Every event in the Kafka payment topic has a corresponding journal entry. Missing entries trigger a P1 alert and automated replay.
3. **External Reconciliation:** Ledger balances are compared against card network clearing files and bank statements daily. Discrepancies are flagged for investigation.

## Multi-Currency Handling

Each account is denominated in a single currency. Cross-currency transactions create entries in two accounts with an exchange rate snapshot:

```
-- EUR 85.00 purchase on a USD card (rate: 1 EUR = 1.1765 USD)
DEBIT   cardholder.available_balance (USD)    $100.00
CREDIT  merchant.pending_auth (EUR)            EUR 85.00
CREDIT  platform.fx_spread_revenue (USD)        $0.50
```

Exchange rates are snapshotted from the rate provider at authorization time and stored with the journal entry. Settlement uses the rate locked at capture time, not the rate at settlement time.

## Scalability

The ledger database is partitioned by `created_at` (monthly partitions). This provides:
- Efficient time-range queries for reporting
- Easy archival of old partitions to cold storage
- Partition pruning for reconciliation queries

At 50,000 TPS, the ledger writes approximately 150,000 journal lines per second (each transaction creates ~3 lines on average). This is handled by:
- Connection pooling (PgBouncer, 200 connections)
- Prepared statements for all write paths
- Batch inserts for journal lines within a single entry
- Read replicas for balance queries and reporting
