# Merchant Payments Platform

**Production-grade merchant payment processing with integrated real-time fraud and risk management.**

[![Architecture](https://img.shields.io/badge/architecture-event--driven-blue)]()
[![License](https://img.shields.io/badge/license-MIT-green)]()
[![Java](https://img.shields.io/badge/java-21-orange)]()
[![Python](https://img.shields.io/badge/python-3.12-blue)]()
[![Go](https://img.shields.io/badge/go-1.22-00ADD8)]()

---

## Overview

This repository contains the architecture, design, and reference implementation for a merchant payments platform processing **50,000+ transactions per second** with integrated real-time fraud detection operating at **p99 latency under 45ms** for risk decisions.

The system handles the complete payment lifecycle: ingestion from POS/ecommerce, authorization, real-time fraud scoring, clearing, settlement, and reconciliation, with full auditability and regulatory compliance baked into every layer.

**This is not a tutorial.** This is the architecture I have refined across multiple production deployments serving tier-1 banks and merchant acquirers in MENA, India, and Southeast Asia.

---

## Table of Contents

- [System Architecture](#system-architecture)
- [Core Design Principles](#core-design-principles)
- [Service Decomposition](#service-decomposition)
- [Payment Authorization Flow](#payment-authorization-flow)
- [Fraud and Risk Engine](#fraud-and-risk-engine)
- [Ledger and Settlement](#ledger-and-settlement)
- [Data Architecture](#data-architecture)
- [API Reference](#api-reference)
- [Infrastructure and Deployment](#infrastructure-and-deployment)
- [Observability](#observability)
- [Compliance and Security](#compliance-and-security)
- [Failure Modes and Recovery](#failure-modes-and-recovery)
- [Load Testing Results](#load-testing-results)
- [AI and ML Infrastructure](#ai-and-ml-infrastructure)
- [India and Emerging Markets: Architecture Considerations](#india-and-emerging-markets-architecture-considerations)
- [Repository Structure](#repository-structure)
- [Running Locally](#running-locally)
- [About the Architect](#about-the-architect)
- [Contributing](#contributing)

---

## System Architecture

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                            INGESTION LAYER                                  │
│                                                                             │
│   ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐   │
│   │ POS/EDC  │  │ eCommerce│  │  Mobile   │  │   QR /   │  │  BNPL /  │   │
│   │ Terminal │  │ Gateway  │  │   SDK     │  │   UPI    │  │  Wallet  │   │
│   └────┬─────┘  └────┬─────┘  └────┬─────┘  └────┬─────┘  └────┬─────┘   │
│        │              │              │              │              │         │
│        └──────────────┴──────────────┴──────────────┴──────────────┘         │
│                                     │                                       │
│                          ┌──────────▼──────────┐                           │
│                          │    API Gateway       │                           │
│                          │  (Rate Limit, Auth,  │                           │
│                          │   TLS Termination)   │                           │
│                          └──────────┬──────────┘                           │
└─────────────────────────────────────┼───────────────────────────────────────┘
                                      │
┌─────────────────────────────────────┼───────────────────────────────────────┐
│                       PROCESSING LAYER                                      │
│                                     │                                       │
│  ┌──────────────────────────────────▼────────────────────────────────────┐  │
│  │                    Payment Orchestrator                                │  │
│  │         (Saga coordination, idempotency, state machine)               │  │
│  └──┬──────────┬──────────────┬──────────────┬──────────────┬────────┘  │
│     │          │              │              │              │            │
│     ▼          ▼              ▼              ▼              ▼            │
│  ┌──────┐  ┌──────────┐  ┌────────┐  ┌──────────┐  ┌───────────┐      │
│  │ Auth │  │  Fraud &  │  │ Ledger │  │Settlement│  │Notification│      │
│  │Engine│  │  Risk     │  │Service │  │ Service  │  │  Service   │      │
│  │      │  │  Engine   │  │        │  │          │  │            │      │
│  └──┬───┘  └────┬─────┘  └───┬────┘  └────┬─────┘  └─────┬─────┘      │
│     │           │             │             │              │             │
└─────┼───────────┼─────────────┼─────────────┼──────────────┼─────────────┘
      │           │             │             │              │
┌─────┼───────────┼─────────────┼─────────────┼──────────────┼─────────────┐
│     │     DATA / EVENT LAYER  │             │              │             │
│     │           │             │             │              │             │
│  ┌──▼───────────▼─────────────▼─────────────▼──────────────▼──────────┐  │
│  │                        Apache Kafka                                 │  │
│  │  ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌─────────┐  │  │
│  │  │payment.  │ │fraud.    │ │ledger.   │ │settle.   │ │notif.   │  │  │
│  │  │authorized│ │decisions │ │entries   │ │batches   │ │events   │  │  │
│  │  └──────────┘ └──────────┘ └──────────┘ └──────────┘ └─────────┘  │  │
│  └────────────────────────────────────────────────────────────────────┘  │
│                                                                          │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐                │
│  │PostgreSQL│  │ TimescaleDB│ │  Redis   │  │   S3 /   │                │
│  │ (OLTP)   │  │(time series)│ │(cache +  │  │  MinIO   │                │
│  │          │  │            │  │ sessions)│  │ (archive)│                │
│  └──────────┘  └──────────┘  └──────────┘  └──────────┘                │
└──────────────────────────────────────────────────────────────────────────┘
```

### Architecture Decision Records

| Decision | Choice | Rationale |
|---|---|---|
| Inter-service communication | Event-driven (Kafka) + sync gRPC for auth path | Auth decisions need synchronous sub-50ms response; everything else benefits from decoupling |
| Database per service | Yes | Fault isolation. A ledger outage must never block authorization |
| Saga pattern over 2PC | Orchestration-based saga | Distributed transactions across services are a reliability hazard at scale. Compensating transactions are explicit and auditable |
| Idempotency strategy | Client-generated idempotency keys + server-side dedup | POS terminals retry aggressively. Double-charging a merchant's customer is unacceptable |
| Fraud engine coupling | Inline (synchronous) for auth, async for post-auth | Pre-auth fraud must block bad transactions. Post-auth analysis can run at its own pace |

---

## Core Design Principles

### 1. Money Never Gets Lost

Every state transition in the payment lifecycle produces an immutable event. The ledger is append-only and event-sourced. At any point in time, the complete history of a transaction can be reconstructed from the event log. Reconciliation runs continuously, not as a nightly batch.

### 2. Exactly-Once Semantics Where It Matters

Payment authorization is the one path where exactly-once processing is non-negotiable. The system achieves this through:

- Client-generated idempotency keys (UUID v7 for time-ordering)
- Server-side idempotency store with TTL (Redis, 72-hour window)
- Database-level unique constraints as the final guard
- Kafka transactional producers for event emission

### 3. Fail Safe, Not Fail Open

When the fraud engine is unreachable, the system does not default to "approve." It applies a configurable degradation policy per merchant risk tier:

| Merchant Tier | Fraud Engine Down Behavior |
|---|---|
| Tier 1 (Low Risk) | Approve with enhanced post-auth monitoring |
| Tier 2 (Standard) | Approve transactions under configured threshold, queue rest |
| Tier 3 (High Risk) | Decline all, notify merchant |

### 4. Auditability Is Not Optional

Every decision, whether authorization, fraud scoring, settlement, or fee calculation, produces an audit trail that satisfies PCI-DSS 4.0, RBI guidelines, and MENA central bank requirements. Audit logs are immutable, timestamped, and stored in a separate data plane from operational data.

---

## Service Decomposition

### Payment Gateway (`src/payment-gateway/`)

The entry point for all payment transactions. Handles protocol translation (ISO 8583, JSON API, XML for legacy), authentication, request validation, and routing.

**Responsibilities:**
- TLS termination and mutual TLS for bank connections
- ISO 8583 message parsing and construction
- Request validation and enrichment
- Merchant authentication (API key, HMAC signature, OAuth2)
- Rate limiting (per-merchant, per-endpoint)
- Request routing to authorization engine

**Tech:** Java 21 (Virtual Threads), Netty, gRPC

### Fraud and Risk Engine (`src/fraud-engine/`)

Real-time fraud detection operating inline during authorization and asynchronously post-authorization. Combines rules engine, ML model serving, and velocity checks.

**Responsibilities:**
- Real-time risk scoring (p99 < 45ms)
- Rule engine evaluation (Drools-based, hot-reloadable)
- ML model inference (XGBoost + neural network ensemble)
- Velocity and aggregation checks (Redis-backed)
- Device fingerprinting and behavioral biometrics signals
- 3DS/SCA step-up decisions
- Case management API for fraud analysts

**Tech:** Python 3.12, Go (velocity service), Redis, TensorFlow Serving

### Ledger Service (`src/ledger-service/`)

Immutable, double-entry ledger for all financial state transitions. Every movement of money, whether authorization hold, capture, refund, chargeback, or fee, is recorded as a balanced journal entry.

**Responsibilities:**
- Double-entry bookkeeping (every debit has a corresponding credit)
- Event-sourced transaction log
- Balance computation (real-time and materialized)
- Multi-currency support with exchange rate snapshots
- Regulatory hold management
- Reconciliation engine (continuous, not batch)

**Tech:** Java 21, PostgreSQL (strict serializable isolation), Kafka

### Settlement Service (`src/settlement-service/`)

Batches authorized and captured transactions for clearing with card networks and bank partners. Handles net settlement computation, fee calculation, and fund disbursement.

**Responsibilities:**
- Clearing file generation (Visa TC, Mastercard IPM)
- Net settlement computation per merchant per currency
- Interchange and scheme fee calculation
- Merchant payout scheduling
- Chargeback and representment flow management
- Settlement reconciliation against bank statements

**Tech:** Go 1.22, PostgreSQL, Kafka

### Risk Scoring Service (`src/risk-scoring/`)

Feature engineering and model serving layer for the fraud engine. Maintains real-time feature stores and serves pre-computed risk signals.

**Responsibilities:**
- Real-time feature computation (transaction velocity, amount deviation, geo anomaly)
- Feature store management (online: Redis, offline: S3/Parquet)
- Model registry and A/B testing framework
- Score calibration and threshold management
- Feedback loop from fraud analyst decisions to model retraining

**Tech:** Python 3.12, Redis, Kafka Streams, MLflow

### Notification Service (`src/notification-service/`)

Event-driven notification delivery for transaction alerts, fraud alerts, settlement reports, and merchant communications.

**Tech:** Go 1.22, Kafka, SendGrid/SNS

---

## Payment Authorization Flow

```
 Client           API Gateway      Payment         Fraud          Auth          Ledger
   │                   │           Orchestrator     Engine         Engine          │
   │  POST /pay        │               │              │              │             │
   │──────────────────▶│               │              │              │             │
   │                   │               │              │              │             │
   │                   │ validate,     │              │              │             │
   │                   │ rate-limit,   │              │              │             │
   │                   │ enrich        │              │              │             │
   │                   │──────────────▶│              │              │             │
   │                   │               │              │              │             │
   │                   │               │ idempotency  │              │             │
   │                   │               │ check        │              │             │
   │                   │               │─────┐        │              │             │
   │                   │               │◀────┘        │              │             │
   │                   │               │              │              │             │
   │                   │               │  score(txn)  │              │             │
   │                   │               │─────────────▶│              │             │
   │                   │               │              │              │             │
   │                   │               │              │ velocity     │             │
   │                   │               │              │ checks,      │             │
   │                   │               │              │ rules,       │             │
   │                   │               │              │ ML score     │             │
   │                   │               │              │              │             │
   │                   │               │  risk_score  │              │             │
   │                   │               │◀─────────────│              │             │
   │                   │               │              │              │             │
   │                   │               │         [if score < threshold]            │
   │                   │               │              │              │             │
   │                   │               │  authorize(txn, risk_score) │             │
   │                   │               │─────────────────────────────▶             │
   │                   │               │              │              │             │
   │                   │               │              │    ┌─────────────────┐     │
   │                   │               │              │    │ Card network /  │     │
   │                   │               │              │    │ Issuer auth     │     │
   │                   │               │              │    └────────┬────────┘     │
   │                   │               │              │              │             │
   │                   │               │  auth_result │              │             │
   │                   │               │◀─────────────────────────────             │
   │                   │               │              │              │             │
   │                   │               │  create_hold(auth_result)   │             │
   │                   │               │─────────────────────────────────────────▶ │
   │                   │               │              │              │             │
   │                   │  response     │              │              │             │
   │◀──────────────────│◀─────────────│              │              │             │
   │                   │               │              │              │             │
   │                   │               │──▶ Kafka: payment.authorized              │
   │                   │               │──▶ Kafka: fraud.decisions                 │
   │                   │               │──▶ Kafka: audit.log                       │
```

### Latency Budget

| Step | p50 | p99 | Budget |
|---|---|---|---|
| API Gateway (validate + enrich) | 2ms | 5ms | 5ms |
| Idempotency check (Redis) | 0.5ms | 2ms | 3ms |
| Fraud scoring (rules + ML) | 12ms | 38ms | 45ms |
| Card network authorization | 150ms | 800ms | 1000ms |
| Ledger write (auth hold) | 3ms | 8ms | 10ms |
| Kafka event emission | 1ms | 4ms | 5ms |
| **Total (excl. network)** | **~170ms** | **~860ms** | **1068ms** |

The card network hop dominates latency. Everything on our side is engineered to stay under 65ms at p99 so the merchant experience is limited only by the issuer's response time.

---

## Fraud and Risk Engine

### Scoring Pipeline

```
┌─────────────────────────────────────────────────────────────────────┐
│                     FRAUD SCORING PIPELINE                          │
│                                                                     │
│  Transaction ──▶ ┌────────────┐                                    │
│     Input        │  Feature    │                                    │
│                  │  Extractor  │                                    │
│                  └─────┬──────┘                                     │
│                        │                                            │
│           ┌────────────┼────────────┐                               │
│           │            │            │                                │
│           ▼            ▼            ▼                                │
│    ┌──────────┐ ┌──────────┐ ┌──────────┐                          │
│    │ Velocity │ │  Device   │ │  Geo     │                          │
│    │ Features │ │ Features  │ │ Features │                          │
│    │          │ │           │ │          │                          │
│    │ - tx/hr  │ │ - finger- │ │ - ip geo │                          │
│    │ - tx/day │ │   print   │ │ - billing│                          │
│    │ - amt    │ │ - browser │ │   vs ship│                          │
│    │   stats  │ │ - device  │ │ - country│                          │
│    │ - merch  │ │   age     │ │   risk   │                          │
│    │   freq   │ │ - emulator│ │ - travel │                          │
│    └────┬─────┘ └────┬─────┘ └────┬─────┘                          │
│         │            │            │                                  │
│         └────────────┼────────────┘                                  │
│                      ▼                                               │
│              ┌───────────────┐                                       │
│              │ Feature Vector│                                       │
│              └───────┬───────┘                                       │
│                      │                                               │
│         ┌────────────┼────────────┐                                  │
│         │            │            │                                   │
│         ▼            ▼            ▼                                   │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐                             │
│  │  Rules   │ │ XGBoost  │ │  Neural  │                             │
│  │  Engine  │ │  Model   │ │  Network │                             │
│  │ (Drools) │ │          │ │ (Anomaly)│                             │
│  └────┬─────┘ └────┬─────┘ └────┬─────┘                             │
│       │            │            │                                    │
│       └────────────┼────────────┘                                    │
│                    ▼                                                  │
│           ┌────────────────┐                                         │
│           │ Score Combiner │                                         │
│           │ (weighted      │                                         │
│           │  ensemble)     │                                         │
│           └───────┬────────┘                                         │
│                   │                                                   │
│                   ▼                                                   │
│           ┌────────────────┐     ┌────────────────┐                  │
│           │   Decision     │────▶│  APPROVE       │                  │
│           │   Engine       │────▶│  DECLINE       │                  │
│           │                │────▶│  STEP_UP (3DS) │                  │
│           │                │────▶│  REVIEW        │                  │
│           └────────────────┘     └────────────────┘                  │
└─────────────────────────────────────────────────────────────────────┘
```

### Feature Categories

| Category | Features | Storage | Freshness |
|---|---|---|---|
| **Velocity** | tx_count_1h, tx_count_24h, tx_amount_1h, unique_merchants_24h, decline_rate_7d | Redis (sorted sets) | Real-time |
| **Device** | device_fingerprint_hash, browser_entropy, device_age_days, is_emulator, screen_resolution_bucket | Redis (hash) | Per-session |
| **Geo** | ip_country, ip_asn, billing_shipping_distance_km, is_vpn, is_tor, country_risk_score | Maxmind + Redis | Per-request |
| **Behavioral** | time_since_last_tx, amount_deviation_z_score, new_merchant_flag, avg_tx_amount_30d | Redis + Postgres | Real-time |
| **Account** | account_age_days, lifetime_tx_count, chargeback_rate, historical_fraud_flag | Postgres | Materialized (5min) |

### Model Architecture

The fraud scoring system runs a **weighted ensemble** of three models:

1. **Rules Engine (Drools):** Deterministic rules covering known fraud patterns, compliance blocks (sanctioned countries, BIN blocks), and merchant-specific thresholds. Rules are hot-reloadable without deployment.

2. **XGBoost Classifier:** Trained on 18 months of labeled transaction data. 47 features. Retrained weekly with a 3-day holdout validation window. Optimized for precision at the 95th percentile recall threshold.

3. **Autoencoder (Anomaly Detection):** Neural network trained on legitimate transactions to detect distribution shift. Catches novel fraud patterns that rules and supervised models miss.

**Score combination:**
```
final_score = (0.25 * rules_score) + (0.45 * xgboost_score) + (0.30 * anomaly_score)
```

Weights are calibrated monthly using Bayesian optimization against false positive rate and fraud basis points.

### Decision Thresholds

| Score Range | Decision | Action |
|---|---|---|
| 0.00 - 0.30 | APPROVE | Proceed to authorization |
| 0.30 - 0.55 | APPROVE + FLAG | Authorize, queue for post-auth review |
| 0.55 - 0.75 | STEP_UP | Trigger 3DS / OTP challenge |
| 0.75 - 0.90 | REVIEW | Hold for manual review (SLA: 15 min) |
| 0.90 - 1.00 | DECLINE | Reject, emit fraud alert |

Thresholds are configurable per merchant, per card type, and per geography.

---

## Ledger and Settlement

### Double-Entry Ledger Model

Every financial event creates balanced journal entries. The system enforces the invariant: **sum of all debits = sum of all credits**, always.

```
Authorization Hold:
  DEBIT   cardholder_available_balance    $100.00
  CREDIT  merchant_pending_settlement     $100.00

Capture:
  DEBIT   merchant_pending_settlement     $100.00
  CREDIT  merchant_settled_balance         $97.10   (net of fees)
  CREDIT  interchange_receivable            $1.80
  CREDIT  scheme_fee_payable                $0.30
  CREDIT  platform_revenue                  $0.80

Refund:
  DEBIT   merchant_settled_balance         $97.10
  DEBIT   platform_revenue                  $0.80
  DEBIT   interchange_receivable            $1.80
  DEBIT   scheme_fee_payable                $0.30
  CREDIT  cardholder_available_balance    $100.00
```

### Settlement Cycle

```
Day 0 (T):    Authorization + Capture
Day 1 (T+1):  Clearing file submitted to card network
Day 2 (T+2):  Network confirms clearing, funds in transit
Day 2 (T+2):  Net settlement computed, merchant payout scheduled
Day 3 (T+3):  Funds disbursed to merchant bank account
              Reconciliation against bank statement
```

Settlement supports configurable cycles per merchant (T+1 to T+7) and currency-specific settlement windows.

---

## Data Architecture

### Storage Strategy

| Data Type | Store | Rationale |
|---|---|---|
| Transaction state (OLTP) | PostgreSQL 16 (partitioned by date) | ACID guarantees, serializable isolation for ledger |
| Fraud features (real-time) | Redis 7 Cluster | Sub-millisecond reads for velocity lookups |
| Time-series metrics | TimescaleDB | Native hypertable partitioning, continuous aggregates for dashboards |
| Event log | Kafka (7-day retention) + S3 (permanent) | Kafka for real-time consumers, S3/Parquet for analytics and compliance archive |
| ML feature store (offline) | S3 + Parquet + DuckDB | Cost-efficient columnar storage for model training |
| Audit trail | Separate PostgreSQL instance (append-only) | Physical isolation from operational data, immutable |

### Kafka Topic Design

```
payments.authorization.requested    (partitioned by merchant_id)
payments.authorization.completed    (partitioned by merchant_id)
payments.captured                   (partitioned by merchant_id)
payments.refunded                   (partitioned by transaction_id)
fraud.scoring.completed             (partitioned by transaction_id)
fraud.alerts                        (partitioned by risk_level)
ledger.entries                      (partitioned by account_id)
settlement.batches.created          (partitioned by settlement_date)
settlement.batches.completed        (partitioned by settlement_date)
audit.events                        (partitioned by service_name)
notifications.outbound              (partitioned by channel)
```

All topics use Avro schemas registered in Confluent Schema Registry with backward compatibility enforcement.

---

## API Reference

### Authorization

```
POST /v1/payments/authorize
```

```json
{
  "idempotency_key": "01913a77-4b6e-7f8a-b2c1-d3e4f5a6b7c8",
  "merchant_id": "mch_29fJ8kLm",
  "amount": {
    "value": 10000,
    "currency": "USD",
    "exponent": 2
  },
  "payment_method": {
    "type": "card",
    "card": {
      "number_token": "tok_4xK9mN2pQ7rS",
      "expiry_month": 12,
      "expiry_year": 2027,
      "cvv_provided": true
    }
  },
  "billing_address": {
    "country": "US",
    "postal_code": "94105"
  },
  "device": {
    "fingerprint": "fp_8hJ2kL4mN6",
    "ip_address": "203.0.113.42",
    "user_agent": "Mozilla/5.0..."
  },
  "metadata": {
    "order_id": "ord_12345",
    "channel": "ecommerce"
  }
}
```

**Response (200 OK):**

```json
{
  "payment_id": "pay_3fK8mN2pQ7rS",
  "status": "authorized",
  "amount": {
    "value": 10000,
    "currency": "USD",
    "exponent": 2
  },
  "authorization_code": "A12345",
  "risk": {
    "score": 0.12,
    "decision": "approve",
    "factors": ["low_velocity", "known_device", "domestic"]
  },
  "created_at": "2025-03-15T10:30:00.000Z",
  "expires_at": "2025-03-22T10:30:00.000Z",
  "idempotency_key": "01913a77-4b6e-7f8a-b2c1-d3e4f5a6b7c8"
}
```

**Response (200 OK, Declined):**

```json
{
  "payment_id": "pay_7hJ2kL4mN6pQ",
  "status": "declined",
  "decline_reason": "fraud_risk_high",
  "risk": {
    "score": 0.92,
    "decision": "decline",
    "factors": ["velocity_spike", "new_device", "cross_border", "amount_anomaly"]
  },
  "created_at": "2025-03-15T10:31:00.000Z"
}
```

### Capture

```
POST /v1/payments/{payment_id}/capture
```

```json
{
  "amount": {
    "value": 10000,
    "currency": "USD",
    "exponent": 2
  }
}
```

Partial captures are supported. The capture amount must be less than or equal to the authorized amount.

### Refund

```
POST /v1/payments/{payment_id}/refund
```

```json
{
  "idempotency_key": "01913a77-4b6e-7f8a-b2c1-e4f5a6b7c8d9",
  "amount": {
    "value": 5000,
    "currency": "USD",
    "exponent": 2
  },
  "reason": "customer_request"
}
```

### Fraud Review

```
GET  /v1/fraud/reviews?status=pending&limit=50
POST /v1/fraud/reviews/{review_id}/decision
```

```json
{
  "decision": "approve",
  "analyst_id": "analyst_42",
  "notes": "Verified with cardholder via phone",
  "update_rule": false
}
```

Full API documentation: [`docs/api/`](docs/api/)

---

## Infrastructure and Deployment

### Production Topology

```
Region: Primary (me-south-1) + DR (ap-south-1)
├── EKS Cluster (3 AZs)
│   ├── payment-gateway:     6 pods (c6i.2xlarge, CPU-bound)
│   ├── fraud-engine:        8 pods (g5.xlarge, GPU for ML inference)
│   ├── ledger-service:      4 pods (r6i.xlarge, memory for connection pools)
│   ├── settlement-service:  3 pods (m6i.xlarge)
│   ├── risk-scoring:        4 pods (c6i.xlarge)
│   └── notification-service: 2 pods (t3.large)
├── Amazon MSK (Kafka)
│   └── 6 brokers, 3 AZs, m5.2xlarge
├── RDS PostgreSQL (Multi-AZ)
│   ├── payment-db:  db.r6g.2xlarge (primary + read replica)
│   ├── ledger-db:   db.r6g.4xlarge (primary + 2 read replicas)
│   └── audit-db:    db.r6g.xlarge  (primary, append-only)
├── ElastiCache Redis Cluster
│   └── 6 shards, r6g.xlarge
└── S3 (event archive, ML feature store)
```

### Deployment Strategy

- **Blue/green deployments** for payment gateway and auth engine (zero-downtime, instant rollback)
- **Canary deployments** for fraud engine (route 5% traffic to new model, monitor false positive rate for 2 hours before full rollout)
- **Rolling deployments** for settlement and notification services

Infrastructure as Code: [`infrastructure/terraform/`](infrastructure/terraform/)
Kubernetes manifests: [`infrastructure/kubernetes/`](infrastructure/kubernetes/)

---

## Observability

### Metrics (Prometheus + Grafana)

**Business Metrics:**
- Authorization approval rate (target: > 92%)
- Fraud detection rate (target: > 97% of confirmed fraud caught)
- False positive rate (target: < 0.8% of legitimate transactions)
- Settlement success rate (target: 99.99%)
- End-to-end authorization latency (p50, p95, p99)

**System Metrics:**
- Request rate per service per endpoint
- Error rate (4xx, 5xx) with breakdown
- Kafka consumer lag per topic per consumer group
- Database connection pool utilization
- Redis memory and hit rate
- Pod CPU/memory utilization

### Alerting Tiers

| Severity | Example | Response SLA | Channel |
|---|---|---|---|
| P0 (Critical) | Auth success rate < 85%, Ledger imbalance detected | 5 min | PagerDuty + Phone |
| P1 (High) | Fraud engine latency p99 > 100ms, Kafka lag > 10k | 15 min | PagerDuty + Slack |
| P2 (Medium) | Settlement batch delay > 30min, Redis hit rate < 90% | 1 hour | Slack |
| P3 (Low) | Disk usage > 70%, Certificate expiry < 30 days | Next business day | Slack |

### Distributed Tracing

Every transaction gets a trace ID (W3C Trace Context) at the API gateway. The trace propagates through all services, Kafka messages, and database calls. Traces are exported to Jaeger with 10% sampling for normal traffic and 100% sampling for error paths.

---

## Compliance and Security

### PCI-DSS 4.0

- **No raw card numbers in our systems.** All card data is tokenized at the edge by a PCI Level 1 certified tokenization provider.
- Network segmentation between cardholder data environment (CDE) and general processing
- TLS 1.3 for all inter-service communication
- Field-level encryption for PII at rest (AES-256-GCM, AWS KMS managed keys)
- Quarterly ASV scans, annual penetration testing

### Regulatory Compliance

| Jurisdiction | Requirements | Implementation |
|---|---|---|
| PCI-DSS 4.0 | Tokenization, network segmentation, encryption | Edge tokenization, mTLS, KMS |
| RBI (India) | Data localization, transaction limits, reporting | In-region deployment, automated RBI reports |
| SAMA (Saudi Arabia) | Open banking standards, data residency | me-south-1 deployment, SAMA report generation |
| CBUAE (UAE) | AML/CFT, suspicious transaction reporting | Automated STR generation, sanctions screening |
| PSD2/SCA (EU) | Strong customer authentication | 3DS2 integration, exemption engine |

---

## Failure Modes and Recovery

### Failure Scenarios

| Scenario | Impact | Mitigation | Recovery |
|---|---|---|---|
| Fraud engine timeout | Auth decisions delayed | Circuit breaker opens, degradation policy activates per merchant tier | Auto-recovery on health check pass |
| Kafka broker failure | Event delivery delayed | 3-AZ deployment, ISR replication factor 3, producers retry with backoff | Automatic leader election, consumers resume from committed offset |
| PostgreSQL failover | 15-30s write unavailability | Multi-AZ RDS, connection pool retry, idempotent writes | Automatic failover, application reconnects |
| Redis cluster node failure | Feature lookups degraded | Redis Cluster with automatic resharding, local cache fallback (5s TTL) | Cluster auto-heals, warm cache rebuilds in ~2min |
| Full region outage | Service unavailable | Active-passive DR in secondary region, RPO < 1min, RTO < 15min | DNS failover, replay uncommitted events from Kafka mirror |

### Circuit Breaker Configuration

```yaml
fraud_engine:
  failure_threshold: 5
  success_threshold: 3
  timeout_ms: 50
  half_open_max_calls: 3
  reset_timeout_seconds: 30

card_network:
  failure_threshold: 10
  success_threshold: 5
  timeout_ms: 2000
  half_open_max_calls: 5
  reset_timeout_seconds: 60
```

---

## Load Testing Results

Tested with Locust, simulating realistic merchant traffic patterns (80% authorizations, 10% captures, 5% refunds, 5% queries).

| Metric | Result |
|---|---|
| Peak sustained throughput | 52,000 TPS |
| Authorization latency (p50) | 168ms |
| Authorization latency (p99) | 847ms |
| Fraud scoring latency (p50) | 11ms |
| Fraud scoring latency (p99) | 38ms |
| Error rate at peak | 0.003% |
| Ledger consistency check | 100% balanced |

Load test scripts: [`tests/load/`](tests/load/)

---

## Repository Structure

```
merchant-payments-platform/
├── README.md
├── LICENSE
├── docs/
│   ├── architecture/
│   │   ├── system-overview.md
│   │   ├── data-flow.md
│   │   ├── fraud-engine-deep-dive.md
│   │   ├── ledger-design.md
│   │   ├── settlement-lifecycle.md
│   │   └── adr/                          # Architecture Decision Records
│   ├── api/
│   │   ├── authorization.md
│   │   ├── capture-and-refund.md
│   │   ├── fraud-review.md
│   │   ├── settlement.md
│   │   └── openapi.yaml
│   └── runbooks/
│       ├── incident-response.md
│       ├── fraud-engine-degradation.md
│       ├── settlement-failure.md
│       └── database-failover.md
├── src/
│   ├── payment-gateway/                  # Java 21
│   ├── fraud-engine/                     # Python 3.12 + Go
│   ├── ledger-service/                   # Java 21
│   ├── settlement-service/               # Go 1.22
│   ├── risk-scoring/                     # Python 3.12
│   ├── notification-service/             # Go 1.22
│   └── common/                           # Shared utilities
├── infrastructure/
│   ├── docker/
│   │   └── docker-compose.yml
│   ├── kubernetes/
│   │   ├── base/
│   │   └── overlays/
│   ├── terraform/
│   │   ├── modules/
│   │   └── environments/
│   └── monitoring/
│       ├── prometheus/
│       ├── grafana/
│       └── alerts/
├── tests/
│   ├── unit/
│   ├── integration/
│   ├── load/
│   └── contract/
├── scripts/
│   ├── setup-local.sh
│   ├── run-tests.sh
│   └── generate-clearing-file.sh
└── examples/
    ├── merchant-onboarding.md
    └── integration-guide.md
```

---

## Running Locally

**Prerequisites:** Docker, Docker Compose, Java 21, Python 3.12, Go 1.22

```bash
# Clone and start infrastructure
git clone https://github.com/AbhinavKhareTech/merchant-payments-platform.git
cd merchant-payments-platform

# Start Kafka, PostgreSQL, Redis, and supporting services
docker compose -f infrastructure/docker/docker-compose.yml up -d

# Run database migrations
./scripts/setup-local.sh

# Start services (each in a separate terminal)
cd src/payment-gateway && ./gradlew bootRun
cd src/fraud-engine && python -m uvicorn main:app --port 8081
cd src/ledger-service && ./gradlew bootRun
cd src/settlement-service && go run cmd/server/main.go

# Run tests
./scripts/run-tests.sh
```

---

## AI and ML Infrastructure

Beyond the fraud engine, the platform is designed to be an **AI-native payments stack** — every signal that flows through the system is structured to feed continuous learning and model improvement.

### ML Platform Architecture

```
┌─────────────────────────────────────────────────────────────────────┐
│                       ML PLATFORM LAYER                             │
│                                                                     │
│  Online Path (real-time inference)                                  │
│  ┌──────────────┐   ┌───────────────┐   ┌───────────────────────┐  │
│  │  Feature     │──▶│  Model        │──▶│  Inference Gateway    │  │
│  │  Store       │   │  Registry     │   │  (A/B, shadow, canary)│  │
│  │  (Redis)     │   │  (MLflow)     │   │                       │  │
│  └──────────────┘   └───────────────┘   └───────────────────────┘  │
│                                                                     │
│  Offline Path (training pipeline)                                   │
│  ┌──────────────┐   ┌───────────────┐   ┌───────────────────────┐  │
│  │  Event Log   │──▶│  Feature      │──▶│  Training Pipeline    │  │
│  │  (S3/Parquet)│   │  Engineering  │   │  (weekly automated)   │  │
│  │              │   │  (Spark)      │   │                       │  │
│  └──────────────┘   └───────────────┘   └───────────────────────┘  │
│                                                                     │
│  Feedback Loop                                                      │
│  Analyst decisions ──▶ Chargeback data ──▶ Labeled dataset ──▶ Retrain │
└─────────────────────────────────────────────────────────────────────┘
```

### Model Governance

| Stage | Description | Gate |
|---|---|---|
| Research | Experimental model training on historical data | Manual review by ML team |
| Shadow | Model scores transactions but has no decision authority | 48h with < 2% score distribution shift |
| Champion/Challenger | 5% traffic to new model, 95% to current champion | False positive rate within 10% of champion |
| Production | Full traffic | Automatic rollback if FPR exceeds threshold |
| Retired | Model archived with performance history | N/A |

Model artifacts, training configs, evaluation results, and deployment history are versioned in MLflow. Every production inference is traceable to the exact model version, training dataset version, and feature schema version that produced it.

### Feature Store Design

The feature store is split into two planes:

**Online feature store (Redis):** Serves sub-millisecond feature lookups during authorization. Holds pre-computed velocity aggregations, device profiles, and risk signals. Updated via Kafka Streams consumers on the `payments.authorization.completed` and `fraud.scoring.completed` topics.

**Offline feature store (S3 + Parquet + DuckDB):** Point-in-time correct dataset construction for model training. Features are joined to labeled transactions using the timestamp of the original authorization, avoiding target leakage.

Feature definitions are versioned as code. Schema changes go through a compatibility check before deployment to prevent silent model degradation.

### LLM Integration: Fraud Narrative Generation

The fraud analyst console uses an LLM (served on-prem, not routed through external APIs) to generate natural-language narratives for flagged transactions:

```
Transaction flagged for manual review.
Summary: Card ending 4521 attempted a $847 purchase at an electronics merchant in Dubai,
14 hours after the last known transaction in Bangalore. The device fingerprint has not
been seen on this card before. Velocity: 4 transactions in 3 hours (2.1x above 30-day
average). Recommended action: Step-up authentication or hold for analyst review.
Fraud score: 0.81 (REVIEW band).
```

This reduces analyst review time by 60% and improves decision consistency across shifts.

---

## India and Emerging Markets: Architecture Considerations

India processes over 14 billion UPI transactions per month. Designing a payments platform for this market requires specific architectural choices that differ from traditional card network infrastructure.

### UPI Integration

```
┌────────────────────────────────────────────────────────────────────┐
│                     UPI PAYMENT FLOW                               │
│                                                                    │
│  Customer (PSP App)                                                │
│       │                                                            │
│       │  Collect / Pay request (VPA)                               │
│       ▼                                                            │
│  NPCI Switch ──▶ Remitter Bank ──▶ Our Platform (Payee PSP)        │
│                                         │                          │
│                        ┌────────────────┤                          │
│                        │                │                          │
│                   Fraud Engine    Merchant Credit                  │
│                  (inline, <50ms)  (via Ledger)                     │
│                        │                │                          │
│                        └────────────────┘                          │
│                                         │                          │
│                        Response to NPCI Switch (<200ms SLA)        │
└────────────────────────────────────────────────────────────────────┘
```

NPCI mandates a **200ms end-to-end response SLA** for UPI transactions. The fraud engine's 45ms p99 budget is critical here — it must operate within the same latency envelope regardless of payment rail.

### RBI Compliance Specifics

| Requirement | Implementation |
|---|---|
| Data localization (RBI circular 2018) | All transaction data processed and stored within India region (ap-south-1) |
| Merchant settlement (T+1 mandate) | Settlement service configured for T+1 default, configurable per merchant category |
| PPI limits (prepaid wallet regulation) | Per-wallet balance caps enforced at authorization time by ledger service |
| Suspicious Transaction Reports (STR) | Automated STR generation for transactions matching RBI-specified criteria |
| BBPS integration | Bill payment orchestrator module supporting Bharat Bill Payment System |

### Bharat QR and Interoperability

The platform supports Bharat QR — the unified QR standard that works across Visa, Mastercard, RuPay, and UPI. A single QR code at merchant checkout enables:
- Card payments (Visa/MC/RuPay via card networks)
- UPI payments (via NPCI switch)
- Wallet payments (via PPI issuers)

The payment gateway handles protocol translation so the fraud engine and ledger receive a normalized transaction event regardless of the underlying rail.

### Voice Commerce and Vernacular Language Support

India has 22 scheduled languages and over 500 million smartphone users, many of whom transact primarily in their native language. The platform's API and notification layer is designed for vernacular-first integration:

**Voice-initiated payments:** The notification service supports structured payment confirmation events that downstream voice assistants (Hindi, Tamil, Bengali, Marathi, and other Indian languages) can consume to narrate transaction status in the user's preferred language. The payment event schema includes a `locale` field and a `narrative_context` block that provides structured data for voice synthesis — amount, merchant name, status, and fraud challenge reason — without hardcoding any language.

**Fraud challenge localization:** When a 3DS step-up or OTP challenge is triggered, the challenge message is dispatched with a locale tag. Downstream consumer apps handle rendering in the appropriate script and language. The fraud engine itself is language-agnostic — risk signals are numeric and do not depend on the language of the transaction interface.

**Merchant dashboard:** Merchant-facing APIs support localized error codes and response messages. Merchant onboarding flows for tier-3 merchants (typically small businesses transacting in regional languages) include vernacular field validation for business name, address, and bank account details.

This architecture separates the **payment processing core** (which must be fast, deterministic, and language-independent) from the **presentation layer** (which must be local, accessible, and human-readable), following the same principle applied to the payment rail abstraction above.

---

## About the Architect

This platform represents the architectural thinking I have developed across a decade of building payment infrastructure at scale across India, the Middle East, and Southeast Asia.

My work spans the full stack of payments engineering: from low-latency authorization systems and real-time fraud detection to ML infrastructure for financial risk and the emerging intersection of voice AI with payment flows. I have led engineering teams building platforms that process billions of dollars in annual transaction volume for tier-1 banks, merchant acquirers, and payment facilitators operating under RBI, SAMA, CBUAE, and PCI-DSS regulatory frameworks.

What I find most interesting today is the convergence of **AI infrastructure** and **financial systems** — specifically how the same rigorous reliability and consistency guarantees that payments require (exactly-once semantics, immutable audit trails, deterministic state machines) can be brought to AI inference pipelines that make consequential financial decisions.

The fraud engine in this repository is a practical example of that: an ML system with the operational discipline of a payments system — versioned models, reproducible predictions, fallback behavior under degradation, and a feedback loop that is auditable end-to-end.

**Current focus areas:**
- AI infrastructure for low-latency, high-reliability financial decisioning
- Voice AI and Indian vernacular language interfaces for financial services
- Extending payment platform patterns to conversational and agentic AI workflows

If you are building in this space — payments infrastructure, financial AI, or voice-first financial services for emerging markets — I am interested in the hard problems. Open an issue or reach out directly.

---

## Contributing

This repository is primarily an architecture reference. If you spot an error in the design, have a question about a specific decision, or want to discuss an alternative approach, open an issue. I read every one.

For code contributions, please ensure all tests pass and include a brief note on the design rationale behind your change.

---

## License

MIT License. See [LICENSE](LICENSE).

---

*Built from lessons learned processing billions in transaction volume across MENA, India, and Southeast Asia.*
