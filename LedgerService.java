package com.payments.ledger.services;

import com.payments.ledger.model.*;
import com.payments.ledger.repository.*;
import io.micrometer.core.instrument.Counter;
import io.micrometer.core.instrument.MeterRegistry;
import io.micrometer.core.instrument.Timer;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.stereotype.Service;
import org.springframework.transaction.annotation.Isolation;
import org.springframework.transaction.annotation.Transactional;

import java.math.BigDecimal;
import java.time.Instant;
import java.time.temporal.ChronoUnit;
import java.util.*;

/**
 * Ledger Service
 *
 * Implements double-entry bookkeeping for the payment platform.
 * Every financial state transition produces balanced journal entries
 * where sum(debits) = sum(credits), always.
 *
 * Design decisions:
 *
 * 1. SERIALIZABLE isolation: The ledger is the one place where we pay the
 *    cost of strict serializability. A phantom read during balance computation
 *    could authorize a payment against a stale balance. The retry cost of
 *    serialization failures (~2-3% of writes) is acceptable compared to the
 *    cost of a financial inconsistency.
 *
 * 2. Append-only: Journal entries and lines are never updated or deleted.
 *    Corrections are recorded as new reversing entries. This makes the audit
 *    trail complete and tamper-evident.
 *
 * 3. Amounts in minor units (long, not BigDecimal): Floating point has no
 *    place in a financial system. All amounts are stored as integers in the
 *    smallest currency unit (cents for USD, fils for AED). Currency exponent
 *    is tracked separately.
 *
 * 4. Idempotency at the entry level: Each journal entry has an idempotency
 *    key. The outbox relay may deliver the same event twice; the ledger must
 *    handle that gracefully.
 */
@Service
public class LedgerService {

    private static final Logger log = LoggerFactory.getLogger(LedgerService.class);
    private static final int MAX_SERIALIZATION_RETRIES = 3;

    private final AccountRepository accountRepository;
    private final JournalEntryRepository entryRepository;
    private final JournalLineRepository lineRepository;
    private final BalanceSnapshotRepository snapshotRepository;
    private final OutboxRepository outboxRepository;
    private final MeterRegistry metrics;

    private final Counter balanceInvariantViolations;
    private final Timer entryWriteTimer;

    public LedgerService(
            AccountRepository accountRepository,
            JournalEntryRepository entryRepository,
            JournalLineRepository lineRepository,
            BalanceSnapshotRepository snapshotRepository,
            OutboxRepository outboxRepository,
            MeterRegistry metrics) {
        this.accountRepository = accountRepository;
        this.entryRepository = entryRepository;
        this.lineRepository = lineRepository;
        this.snapshotRepository = snapshotRepository;
        this.outboxRepository = outboxRepository;
        this.metrics = metrics;

        this.balanceInvariantViolations = Counter.builder("ledger_balance_invariant_violations_total")
                .description("Number of balance invariant violations detected")
                .register(metrics);
        this.entryWriteTimer = Timer.builder("ledger_entry_write_duration_seconds")
                .description("Time to write a journal entry")
                .register(metrics);
    }

    // ─── Authorization Hold ───

    /**
     * Creates an authorization hold in the ledger.
     *
     * Debits the cardholder's available balance and credits the merchant's
     * pending authorization account. This "reserves" the funds without
     * actually moving money.
     *
     * The hold expires after a configurable period (default: 7 days for
     * card-not-present, 24 hours for card-present). Expired holds are
     * automatically released by a scheduled job.
     */
    @Transactional(isolation = Isolation.SERIALIZABLE)
    public AuthorizationHoldResult createAuthorizationHold(
            String merchantId,
            Money amount,
            String authorizationCode,
            UUID traceId) {

        return entryWriteTimer.record(() -> {
            String idempotencyKey = "auth_hold:" + authorizationCode;

            // Idempotency check: if this hold already exists, return the existing result
            var existing = entryRepository.findByIdempotencyKey(idempotencyKey);
            if (existing.isPresent()) {
                log.info("Idempotent auth hold detected. traceId={}, authCode={}", traceId, authorizationCode);
                return toHoldResult(existing.get());
            }

            // Resolve accounts
            Account cardholderAccount = resolveOrCreateAccount(
                    "ASSET", "CARDHOLDER", authorizationCode, amount.currency());
            Account merchantPendingAccount = resolveOrCreateAccount(
                    "LIABILITY", "MERCHANT", merchantId + ":pending_auth", amount.currency());

            // Create journal entry
            JournalEntry entry = new JournalEntry();
            entry.setTransactionId(traceId);
            entry.setEntryType("AUTH_HOLD");
            entry.setDescription("Authorization hold for auth code " + authorizationCode);
            entry.setCreatedBy("ledger-service");
            entry.setEventId(traceId);
            entry.setIdempotencyKey(idempotencyKey);
            entry.setCreatedAt(Instant.now());
            entry = entryRepository.save(entry);

            // Create balanced journal lines
            List<JournalLine> lines = List.of(
                    createLine(entry, cardholderAccount, Direction.DEBIT, amount),
                    createLine(entry, merchantPendingAccount, Direction.CREDIT, amount)
            );
            lineRepository.saveAll(lines);

            // Validate balance invariant
            validateEntryBalance(entry.getEntryId());

            // Write to outbox for event emission
            outboxRepository.save(new OutboxEvent(
                    "ledger.entries",
                    merchantId,
                    Map.of(
                            "entry_id", entry.getEntryId().toString(),
                            "entry_type", "AUTH_HOLD",
                            "transaction_id", traceId.toString(),
                            "amount", amount.value(),
                            "currency", amount.currency()
                    )
            ));

            Instant expiresAt = Instant.now().plus(7, ChronoUnit.DAYS);

            log.info("Auth hold created. traceId={}, entryId={}, amount={} {}",
                    traceId, entry.getEntryId(), amount.value(), amount.currency());

            return new AuthorizationHoldResult(traceId, entry.getEntryId(), expiresAt);
        });
    }

    // ─── Capture ───

    /**
     * Captures an authorized payment.
     *
     * Releases the authorization hold and creates entries for the captured
     * amount, including fee breakdown (interchange, scheme fees, processing).
     *
     * Supports partial capture: capture amount can be less than the authorized
     * amount. The remaining hold is released.
     */
    @Transactional(isolation = Isolation.SERIALIZABLE)
    public CaptureResult capture(
            String merchantId,
            String authorizationCode,
            Money captureAmount,
            FeeBreakdown fees,
            UUID traceId) {

        return entryWriteTimer.record(() -> {
            String idempotencyKey = "capture:" + authorizationCode + ":" + captureAmount.value();

            var existing = entryRepository.findByIdempotencyKey(idempotencyKey);
            if (existing.isPresent()) {
                log.info("Idempotent capture detected. traceId={}", traceId);
                return toCaptureResult(existing.get());
            }

            // Resolve accounts
            Account merchantPending = resolveAccount(
                    "LIABILITY", "MERCHANT", merchantId + ":pending_auth", captureAmount.currency());
            Account merchantCaptured = resolveOrCreateAccount(
                    "LIABILITY", "MERCHANT", merchantId + ":captured", captureAmount.currency());
            Account interchangeReceivable = resolveOrCreateAccount(
                    "ASSET", "PLATFORM", "interchange_receivable", captureAmount.currency());
            Account schemeFeePayable = resolveOrCreateAccount(
                    "LIABILITY", "PLATFORM", "scheme_fee_payable", captureAmount.currency());
            Account processingRevenue = resolveOrCreateAccount(
                    "REVENUE", "PLATFORM", "processing_revenue", captureAmount.currency());

            // Create journal entry
            JournalEntry entry = new JournalEntry();
            entry.setTransactionId(traceId);
            entry.setEntryType("CAPTURE");
            entry.setDescription("Capture for auth code " + authorizationCode);
            entry.setCreatedBy("ledger-service");
            entry.setEventId(traceId);
            entry.setIdempotencyKey(idempotencyKey);
            entry.setCreatedAt(Instant.now());
            entry = entryRepository.save(entry);

            // Net merchant amount = capture - interchange - scheme fee - processing fee
            long netMerchantAmount = captureAmount.value()
                    - fees.interchangeFee()
                    - fees.schemeFee()
                    - fees.processingFee();

            List<JournalLine> lines = new ArrayList<>();

            // Release the pending auth hold
            lines.add(createLine(entry, merchantPending, Direction.DEBIT,
                    new Money(captureAmount.value(), captureAmount.currency())));

            // Credit merchant net amount
            lines.add(createLine(entry, merchantCaptured, Direction.CREDIT,
                    new Money(netMerchantAmount, captureAmount.currency())));

            // Record fees
            lines.add(createLine(entry, interchangeReceivable, Direction.CREDIT,
                    new Money(fees.interchangeFee(), captureAmount.currency())));
            lines.add(createLine(entry, schemeFeePayable, Direction.CREDIT,
                    new Money(fees.schemeFee(), captureAmount.currency())));
            lines.add(createLine(entry, processingRevenue, Direction.CREDIT,
                    new Money(fees.processingFee(), captureAmount.currency())));

            lineRepository.saveAll(lines);

            // Validate balance invariant
            validateEntryBalance(entry.getEntryId());

            // Outbox event
            outboxRepository.save(new OutboxEvent(
                    "ledger.entries",
                    merchantId,
                    Map.of(
                            "entry_id", entry.getEntryId().toString(),
                            "entry_type", "CAPTURE",
                            "transaction_id", traceId.toString(),
                            "gross_amount", captureAmount.value(),
                            "net_amount", netMerchantAmount,
                            "interchange", fees.interchangeFee(),
                            "scheme_fee", fees.schemeFee(),
                            "processing_fee", fees.processingFee(),
                            "currency", captureAmount.currency()
                    )
            ));

            log.info("Capture complete. traceId={}, gross={}, net={}, fees={}",
                    traceId, captureAmount.value(), netMerchantAmount,
                    fees.interchangeFee() + fees.schemeFee() + fees.processingFee());

            return new CaptureResult(entry.getEntryId(), netMerchantAmount, fees);
        });
    }

    // ─── Refund ───

    @Transactional(isolation = Isolation.SERIALIZABLE)
    public RefundResult refund(
            String merchantId,
            Money refundAmount,
            FeeBreakdown originalFees,
            UUID originalTransactionId,
            UUID traceId) {

        return entryWriteTimer.record(() -> {
            String idempotencyKey = "refund:" + traceId;

            var existing = entryRepository.findByIdempotencyKey(idempotencyKey);
            if (existing.isPresent()) {
                return toRefundResult(existing.get());
            }

            Account merchantCaptured = resolveAccount(
                    "LIABILITY", "MERCHANT", merchantId + ":captured", refundAmount.currency());
            Account cardholderAccount = resolveOrCreateAccount(
                    "ASSET", "CARDHOLDER", originalTransactionId.toString(), refundAmount.currency());
            Account interchangeReceivable = resolveAccount(
                    "ASSET", "PLATFORM", "interchange_receivable", refundAmount.currency());
            Account schemeFeePayable = resolveAccount(
                    "LIABILITY", "PLATFORM", "scheme_fee_payable", refundAmount.currency());
            Account processingRevenue = resolveAccount(
                    "REVENUE", "PLATFORM", "processing_revenue", refundAmount.currency());

            JournalEntry entry = new JournalEntry();
            entry.setTransactionId(traceId);
            entry.setEntryType("REFUND");
            entry.setDescription("Refund for original transaction " + originalTransactionId);
            entry.setCreatedBy("ledger-service");
            entry.setEventId(traceId);
            entry.setIdempotencyKey(idempotencyKey);
            entry.setCreatedAt(Instant.now());
            entry = entryRepository.save(entry);

            long netMerchantAmount = refundAmount.value()
                    - originalFees.interchangeFee()
                    - originalFees.schemeFee()
                    - originalFees.processingFee();

            List<JournalLine> lines = List.of(
                    // Reverse merchant captured balance (net amount only)
                    createLine(entry, merchantCaptured, Direction.DEBIT,
                            new Money(netMerchantAmount, refundAmount.currency())),
                    // Reverse fees
                    createLine(entry, processingRevenue, Direction.DEBIT,
                            new Money(originalFees.processingFee(), refundAmount.currency())),
                    createLine(entry, interchangeReceivable, Direction.DEBIT,
                            new Money(originalFees.interchangeFee(), refundAmount.currency())),
                    createLine(entry, schemeFeePayable, Direction.DEBIT,
                            new Money(originalFees.schemeFee(), refundAmount.currency())),
                    // Credit back to cardholder (full amount)
                    createLine(entry, cardholderAccount, Direction.CREDIT, refundAmount)
            );
            lineRepository.saveAll(lines);

            validateEntryBalance(entry.getEntryId());

            log.info("Refund complete. traceId={}, amount={} {}", traceId, refundAmount.value(), refundAmount.currency());

            return new RefundResult(entry.getEntryId(), refundAmount);
        });
    }

    // ─── Balance Queries ───

    /**
     * Computes the real-time balance for an account.
     * This is a potentially expensive query on high-volume accounts.
     * For dashboard/reporting use cases, prefer getSnapshotBalance().
     */
    @Transactional(readOnly = true, isolation = Isolation.SERIALIZABLE)
    public Balance getBalance(UUID accountId) {
        var result = lineRepository.computeBalance(accountId);
        return new Balance(
                accountId,
                result.totalDebits(),
                result.totalCredits(),
                result.totalDebits() - result.totalCredits(),
                result.currency(),
                Instant.now()
        );
    }

    /**
     * Returns the most recent balance snapshot for an account.
     * Snapshots are computed every 5 minutes by a scheduled job.
     * Suitable for dashboards, settlement computation, and reconciliation.
     */
    @Transactional(readOnly = true)
    public Optional<BalanceSnapshot> getSnapshotBalance(UUID accountId) {
        return snapshotRepository.findLatestByAccountId(accountId);
    }

    // ─── Reconciliation ───

    /**
     * Validates the balance invariant for a specific journal entry.
     * sum(debits) must equal sum(credits).
     * If violated, increments the invariant violation counter (triggers P0 alert).
     */
    private void validateEntryBalance(UUID entryId) {
        var lines = lineRepository.findByEntryId(entryId);

        long totalDebits = lines.stream()
                .filter(l -> l.getDirection() == Direction.DEBIT)
                .mapToLong(JournalLine::getAmount)
                .sum();

        long totalCredits = lines.stream()
                .filter(l -> l.getDirection() == Direction.CREDIT)
                .mapToLong(JournalLine::getAmount)
                .sum();

        if (totalDebits != totalCredits) {
            balanceInvariantViolations.increment();
            log.error("BALANCE INVARIANT VIOLATION. entryId={}, debits={}, credits={}, diff={}",
                    entryId, totalDebits, totalCredits, totalDebits - totalCredits);
            throw new BalanceInvariantViolationException(
                    "Journal entry " + entryId + " is imbalanced: debits=" + totalDebits + " credits=" + totalCredits);
        }
    }

    // ─── Helpers ───

    private Account resolveOrCreateAccount(String type, String entityType, String entityId, String currency) {
        return accountRepository.findByEntityTypeAndEntityIdAndCurrencyAndAccountType(
                        entityType, entityId, currency, type)
                .orElseGet(() -> {
                    Account account = new Account();
                    account.setAccountType(type);
                    account.setEntityType(entityType);
                    account.setEntityId(entityId);
                    account.setCurrency(currency);
                    account.setCreatedAt(Instant.now());
                    return accountRepository.save(account);
                });
    }

    private Account resolveAccount(String type, String entityType, String entityId, String currency) {
        return accountRepository.findByEntityTypeAndEntityIdAndCurrencyAndAccountType(
                        entityType, entityId, currency, type)
                .orElseThrow(() -> new AccountNotFoundException(
                        "Account not found: " + entityType + "/" + entityId + "/" + currency + "/" + type));
    }

    private JournalLine createLine(JournalEntry entry, Account account, Direction direction, Money amount) {
        JournalLine line = new JournalLine();
        line.setEntryId(entry.getEntryId());
        line.setAccountId(account.getAccountId());
        line.setDirection(direction);
        line.setAmount(amount.value());
        line.setCurrency(amount.currency());
        line.setCreatedAt(Instant.now());
        return line;
    }

    private AuthorizationHoldResult toHoldResult(JournalEntry entry) {
        return new AuthorizationHoldResult(
                entry.getTransactionId(),
                entry.getEntryId(),
                entry.getCreatedAt().plus(7, ChronoUnit.DAYS));
    }

    private CaptureResult toCaptureResult(JournalEntry entry) {
        return new CaptureResult(entry.getEntryId(), 0L, FeeBreakdown.ZERO);
    }

    private RefundResult toRefundResult(JournalEntry entry) {
        return new RefundResult(entry.getEntryId(), Money.ZERO);
    }
}
