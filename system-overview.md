# System Architecture Overview

## Context

This document describes the architecture of the Merchant Payments Platform, a system designed to process card-present and card-not-present payments for merchant acquirers and payment facilitators.

The platform handles the complete payment lifecycle from transaction ingestion through settlement and reconciliation, with real-time fraud detection integrated into the authorization path.

## Architectural Style

The system follows an **event-driven microservices architecture** with the following characteristics:

- Services are decomposed along domain boundaries (payments, fraud, ledger, settlement)
- Each service owns its data store (database-per-service pattern)
- Synchronous communication (gRPC) is used only on the critical authorization path where latency matters
- All other inter-service communication flows through Apache Kafka
- State changes are captured as immutable events, enabling event sourcing for the ledger and audit trail

## Why Not a Monolith?

At the scale this system targets (50,000+ TPS), the operational concerns of each domain diverge significantly:

- The **payment gateway** is CPU-bound (parsing, validation, TLS) and needs horizontal scaling by adding stateless pods
- The **fraud engine** is compute-bound (ML inference) and benefits from GPU-attached instances
- The **ledger service** is I/O-bound (database writes with strict isolation) and needs careful connection pool management
- The **settlement service** is batch-oriented and runs on a different cadence than the real-time path

A monolith would force all of these to share the same scaling, deployment, and failure characteristics. That is not a trade-off worth making at this scale.

## Service Interaction Patterns

### Synchronous Path (Authorization)

The authorization flow is the only synchronous path in the system. It uses gRPC for internal service calls because:

1. Protocol Buffers provide strict schema enforcement at compile time
2. HTTP/2 multiplexing reduces connection overhead
3. Bidirectional streaming supports future use cases (real-time fraud feedback)
4. Generated client libraries eliminate hand-written HTTP clients

The authorization path touches: API Gateway -> Payment Orchestrator -> Fraud Engine -> Auth Engine -> Ledger Service.

Total internal latency budget: 65ms at p99 (excluding card network round-trip).

### Asynchronous Path (Everything Else)

All non-authorization flows use Kafka:

- Post-authorization fraud analysis
- Ledger entry propagation
- Settlement batch creation
- Notification delivery
- Audit event capture
- ML feature store updates

This decoupling means a slow settlement batch cannot affect authorization throughput, and a notification service outage does not impact payment processing.

## Data Consistency Model

### Authorization Path: Strong Consistency

The authorization path requires strong consistency. A payment must not be authorized twice (idempotency), and the ledger hold must be created atomically with the authorization. This is achieved through:

1. Idempotency key check in Redis (fast path dedup)
2. PostgreSQL unique constraint on idempotency key (durable dedup)
3. Transactional outbox pattern for event emission (write to DB + outbox in one transaction, relay to Kafka asynchronously)

### Post-Authorization: Eventual Consistency

Settlement, reconciliation, and analytics are eventually consistent. The system uses Kafka's ordering guarantees (per-partition ordering by merchant_id or transaction_id) to ensure events are processed in the correct sequence.

Reconciliation runs continuously, comparing ledger state against expected state derived from the event log. Discrepancies trigger P1 alerts.

## Multi-Region Strategy

The platform runs active-passive across two regions:

- **Primary (me-south-1):** Handles all traffic
- **DR (ap-south-1):** Warm standby with continuous replication

Replication approach:
- PostgreSQL: Logical replication for ledger and payment databases
- Kafka: MirrorMaker 2 for event log replication
- Redis: Cross-region replication for fraud feature cache

RPO target: < 1 minute
RTO target: < 15 minutes

Failover is initiated manually (not automatic) because payment systems require human judgment during region failures to avoid split-brain scenarios.

## Security Architecture

### Network Security

```
Internet -> WAF -> NLB -> API Gateway (TLS termination)
                              |
                              v
                    [Service Mesh (Istio)]
                     mTLS between all services
                              |
           +------------------+------------------+
           |                  |                  |
        Services           Kafka              Databases
     (Pod network)    (Private subnet)    (Private subnet,
                                          no internet access)
```

### Data Protection

- Card data is tokenized before entering the system (PCI scope reduction)
- PII fields (name, email, phone) are encrypted at the field level using AES-256-GCM
- Encryption keys are managed through AWS KMS with automatic rotation
- Database connections use TLS with certificate pinning
- Secrets are managed through HashiCorp Vault (not environment variables)

### Authentication and Authorization

- Merchant API authentication: HMAC-SHA256 signed requests with rotating API keys
- Internal service authentication: mTLS with short-lived certificates (Istio)
- Fraud analyst console: OAuth2 + RBAC with principle of least privilege
- All authentication events are logged to the audit trail
