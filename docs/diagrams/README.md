# Architecture Diagrams

Visual documentation for the Merchant Payments Platform. All diagrams use [Mermaid](https://mermaid.js.org/) and render natively on GitHub.

## System Architecture

### [01 - System Architecture Overview](diagrams/01-system-architecture.md)
High-level component diagram showing all services, data stores, event flows, and external integrations.

### [08 - Infrastructure Topology](diagrams/08-infrastructure-topology.md)
AWS production deployment: EKS cluster layout, RDS databases, ElastiCache, MSK, S3, and DR region.

### [13 - Network Security & PCI-DSS](diagrams/13-network-security.md)
Security architecture: WAF, service mesh (mTLS), CDE network segmentation, tokenization, encryption, and secrets management.

## Payment Flows

### [02 - Authorization Flow](diagrams/02-authorization-flow.md)
Complete sequence diagram: API gateway validation through fraud scoring, card network auth, ledger hold, and event emission.

### [04 - Payment State Machine](diagrams/04-payment-state-machine.md)
Full lifecycle state machine: initiated through authorized, captured, refunded, settled, and chargeback paths.

### [14 - Capture & Refund Flows](diagrams/14-capture-refund-flow.md)
Detailed sequence for full/partial capture and refund operations with ledger entry breakdowns.

### [17 - Latency Budget](diagrams/17-latency-budget.md)
Authorization path latency breakdown: per-component budget from API gateway through ledger write.

## Fraud & Risk Engine

### [03 - Fraud Scoring Pipeline](diagrams/03-fraud-scoring-pipeline.md)
Feature extraction (velocity, device, geo, behavioral, account), three-model ensemble (rules, XGBoost, autoencoder), and decision thresholds.

### [09 - Circuit Breaker & Degradation](diagrams/09-circuit-breaker-degradation.md)
Fraud engine circuit breaker states and merchant-tier degradation policies.

### [12 - ML Feedback Loop & Retraining](diagrams/12-ml-feedback-loop.md)
End-to-end pipeline: feedback sources, label pipeline, weekly retraining, champion/challenger deployment, and model monitoring.

## Ledger & Settlement

### [05 - Double-Entry Bookkeeping](diagrams/05-ledger-double-entry.md)
Ledger journal entries for every financial event: auth hold, capture (with fee breakdown), refund, settlement payout, and chargeback.

### [06 - Settlement Lifecycle](diagrams/06-settlement-lifecycle.md)
T+0 through T+3: clearing file generation, network confirmation, net settlement computation, merchant payout, and three-way reconciliation.

### [11 - Chargeback Flow](diagrams/11-chargeback-flow.md)
Complete chargeback lifecycle: initiation, representment, arbitration, and chargeback ratio monitoring.

## Data Architecture

### [07 - Kafka Event Flow](diagrams/07-kafka-event-flow.md)
All Kafka topics, their partitioning strategy, producers, and consumers.

### [10 - Data Storage Architecture](diagrams/10-data-storage-architecture.md)
Storage tiers (hot/warm/stream/cold) with data placement rationale and service access patterns.

### [15 - Entity Relationship Diagram](diagrams/15-entity-relationship.md)
Core data model: merchants, transactions, ledger accounts, journal entries, settlement batches, fraud reviews, and audit events.

## Operations

### [16 - Observability & Alerting](diagrams/16-observability.md)
Metrics (Prometheus/Thanos), traces (Jaeger), logs (ELK), Grafana dashboards, and four-tier alerting pipeline.
