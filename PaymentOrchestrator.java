package com.payments.orchestrator;

import com.payments.orchestrator.model.*;
import com.payments.orchestrator.saga.*;
import com.payments.common.types.*;
import io.grpc.StatusRuntimeException;
import io.micrometer.core.instrument.MeterRegistry;
import io.micrometer.core.instrument.Timer;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import java.time.Duration;
import java.time.Instant;
import java.util.UUID;
import java.util.concurrent.CompletableFuture;

/**
 * Payment Orchestrator
 *
 * Coordinates the authorization saga across fraud engine, auth engine, and ledger service.
 * Implements the orchestration-based saga pattern with explicit compensating transactions.
 *
 * Design decisions:
 * - Orchestrator (not choreography) because payment authorization requires strict ordering
 *   and centralized failure handling. A missed compensation in a choreographed saga means
 *   money gets lost.
 * - Virtual threads (Java 21) for I/O-bound gRPC calls to fraud engine and card network.
 *   No need for reactive complexity when the JVM handles blocking efficiently.
 * - Idempotency is enforced at two levels: Redis (fast path) and PostgreSQL (durable).
 *   POS terminals retry aggressively; double-charging a customer is unacceptable.
 */
public class PaymentOrchestrator {

    private static final Logger log = LoggerFactory.getLogger(PaymentOrchestrator.class);
    private static final Duration FRAUD_ENGINE_TIMEOUT = Duration.ofMillis(50);
    private static final Duration AUTH_ENGINE_TIMEOUT = Duration.ofMillis(2000);
    private static final int MAX_IDEMPOTENCY_RETRIES = 3;

    private final IdempotencyStore idempotencyStore;
    private final FraudEngineClient fraudEngine;
    private final AuthorizationEngine authEngine;
    private final LedgerClient ledger;
    private final EventPublisher eventPublisher;
    private final DegradationPolicy degradationPolicy;
    private final MeterRegistry metrics;

    public PaymentOrchestrator(
            IdempotencyStore idempotencyStore,
            FraudEngineClient fraudEngine,
            AuthorizationEngine authEngine,
            LedgerClient ledger,
            EventPublisher eventPublisher,
            DegradationPolicy degradationPolicy,
            MeterRegistry metrics) {
        this.idempotencyStore = idempotencyStore;
        this.fraudEngine = fraudEngine;
        this.authEngine = authEngine;
        this.ledger = ledger;
        this.eventPublisher = eventPublisher;
        this.degradationPolicy = degradationPolicy;
        this.metrics = metrics;
    }

    /**
     * Executes the payment authorization saga.
     *
     * Steps:
     * 1. Idempotency check (dedup retried requests)
     * 2. Fraud scoring (inline, synchronous)
     * 3. Authorization decision (based on fraud score + merchant config)
     * 4. Card network authorization (if approved)
     * 5. Ledger hold creation (atomic with auth result)
     * 6. Event emission (transactional outbox)
     *
     * Compensation:
     * - If ledger write fails after auth: emit auth reversal to card network
     * - If event emission fails: ledger entry includes outbox record, relay picks it up
     */
    public AuthorizationResult authorize(AuthorizationRequest request) {
        var timer = Timer.builder("payment.authorization.duration")
                .tag("merchant_tier", request.merchantTier().name())
                .register(metrics);

        return timer.record(() -> executeAuthorizationSaga(request));
    }

    private AuthorizationResult executeAuthorizationSaga(AuthorizationRequest request) {
        var traceId = request.traceId();
        var startTime = Instant.now();

        log.info("Starting authorization saga. traceId={}, merchantId={}, amount={} {}",
                traceId, request.merchantId(), request.amount().value(), request.amount().currency());

        // ─── Step 1: Idempotency Check ───
        var existingResult = idempotencyStore.get(request.idempotencyKey());
        if (existingResult.isPresent()) {
            log.info("Idempotent request detected. traceId={}, idempotencyKey={}",
                    traceId, request.idempotencyKey());
            metrics.counter("payment.authorization.idempotent_hit").increment();
            return existingResult.get();
        }

        // ─── Step 2: Fraud Scoring ───
        FraudDecision fraudDecision;
        try {
            fraudDecision = scoreFraud(request);
        } catch (FraudEngineUnavailableException e) {
            log.warn("Fraud engine unavailable. Applying degradation policy. traceId={}", traceId);
            fraudDecision = degradationPolicy.evaluate(request);
            metrics.counter("payment.authorization.fraud_degraded").increment();
        }

        log.info("Fraud scoring complete. traceId={}, score={}, decision={}",
                traceId, fraudDecision.score(), fraudDecision.decision());

        // ─── Step 3: Authorization Decision ───
        if (fraudDecision.decision() == Decision.DECLINE) {
            var result = AuthorizationResult.declined(
                    UUID.randomUUID(),
                    request,
                    fraudDecision,
                    "fraud_risk_high"
            );
            idempotencyStore.put(request.idempotencyKey(), result, Duration.ofHours(72));
            emitEvents(request, result, fraudDecision);
            return result;
        }

        if (fraudDecision.decision() == Decision.STEP_UP) {
            var result = AuthorizationResult.stepUpRequired(
                    UUID.randomUUID(),
                    request,
                    fraudDecision
            );
            idempotencyStore.put(request.idempotencyKey(), result, Duration.ofHours(72));
            emitEvents(request, result, fraudDecision);
            return result;
        }

        // ─── Step 4: Card Network Authorization ───
        NetworkAuthResponse networkResponse;
        try {
            networkResponse = authEngine.authorize(request, fraudDecision);
        } catch (StatusRuntimeException e) {
            log.error("Card network authorization failed. traceId={}, error={}",
                    traceId, e.getStatus());
            var result = AuthorizationResult.declined(
                    UUID.randomUUID(),
                    request,
                    fraudDecision,
                    "network_error"
            );
            idempotencyStore.put(request.idempotencyKey(), result, Duration.ofHours(72));
            emitEvents(request, result, fraudDecision);
            return result;
        }

        if (!networkResponse.approved()) {
            var result = AuthorizationResult.declined(
                    UUID.randomUUID(),
                    request,
                    fraudDecision,
                    networkResponse.declineCode()
            );
            idempotencyStore.put(request.idempotencyKey(), result, Duration.ofHours(72));
            emitEvents(request, result, fraudDecision);
            return result;
        }

        // ─── Step 5: Ledger Hold ───
        LedgerHoldResult holdResult;
        try {
            holdResult = ledger.createAuthorizationHold(
                    request.merchantId(),
                    request.amount(),
                    networkResponse.authorizationCode(),
                    traceId
            );
        } catch (Exception e) {
            // CRITICAL: Auth succeeded at network but ledger write failed.
            // Must reverse the authorization to avoid orphaned holds.
            log.error("Ledger write failed after network auth. Initiating reversal. traceId={}", traceId);
            compensateNetworkAuth(request, networkResponse);
            metrics.counter("payment.authorization.ledger_compensation").increment();
            throw new PaymentProcessingException("Ledger write failed, authorization reversed", e);
        }

        // ─── Step 6: Compose Result and Emit Events ───
        var result = AuthorizationResult.authorized(
                holdResult.paymentId(),
                request,
                fraudDecision,
                networkResponse.authorizationCode(),
                holdResult.expiresAt()
        );

        idempotencyStore.put(request.idempotencyKey(), result, Duration.ofHours(72));
        emitEvents(request, result, fraudDecision);

        var duration = Duration.between(startTime, Instant.now());
        log.info("Authorization saga complete. traceId={}, paymentId={}, status={}, duration={}ms",
                traceId, result.paymentId(), result.status(), duration.toMillis());

        return result;
    }

    private FraudDecision scoreFraud(AuthorizationRequest request) {
        try {
            return fraudEngine.score(request)
                    .orTimeout(FRAUD_ENGINE_TIMEOUT.toMillis(), java.util.concurrent.TimeUnit.MILLISECONDS)
                    .join();
        } catch (Exception e) {
            if (e.getCause() instanceof java.util.concurrent.TimeoutException) {
                metrics.counter("payment.fraud.timeout").increment();
                throw new FraudEngineUnavailableException("Fraud engine timed out", e);
            }
            throw new FraudEngineUnavailableException("Fraud engine error", e);
        }
    }

    private void compensateNetworkAuth(AuthorizationRequest request, NetworkAuthResponse networkResponse) {
        try {
            authEngine.reverseAuthorization(
                    request,
                    networkResponse.authorizationCode(),
                    "ledger_write_failure"
            );
            log.info("Network authorization reversed. traceId={}, authCode={}",
                    request.traceId(), networkResponse.authorizationCode());
        } catch (Exception e) {
            // If reversal also fails, emit to a dead letter queue for manual intervention.
            // This is an extremely rare scenario that requires human attention.
            log.error("CRITICAL: Network auth reversal failed. Manual intervention required. traceId={}",
                    request.traceId(), e);
            eventPublisher.publishToDeadLetter(new AuthReversalFailedEvent(
                    request.traceId(),
                    request.merchantId(),
                    networkResponse.authorizationCode(),
                    request.amount()
            ));
            metrics.counter("payment.authorization.reversal_failed").increment();
        }
    }

    private void emitEvents(AuthorizationRequest request, AuthorizationResult result, FraudDecision fraud) {
        // Events are published via the transactional outbox pattern.
        // The outbox relay (separate thread) reads from the outbox table and publishes to Kafka.
        // This guarantees at-least-once delivery without distributed transactions.
        eventPublisher.publish(new PaymentAuthorizedEvent(request, result));
        eventPublisher.publish(new FraudDecisionEvent(request.traceId(), fraud));
        eventPublisher.publish(new AuditEvent(
                "payment.authorization",
                request.traceId(),
                result.status().name(),
                Instant.now()
        ));
    }
}
