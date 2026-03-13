# Fraud Engine Deep Dive

## Purpose

The fraud engine provides real-time risk assessment for every payment authorization request. It operates inline (synchronous) during the authorization flow and asynchronously for post-authorization monitoring.

The core contract: **return a risk score and decision within 45ms at p99**, with enough signal for the payment orchestrator to make an approve/decline/step-up decision.

## Architecture

The fraud engine is composed of three layers:

### Layer 1: Feature Extraction

When a transaction arrives, the feature extractor pulls signals from multiple sources in parallel:

**Velocity Features (Redis, ~2ms)**
- Transaction count in the last 1h, 6h, 24h for this card
- Cumulative amount in the last 1h, 6h, 24h
- Unique merchant count in the last 24h
- Decline count in the last 7d

**Device Features (Redis, ~1ms)**
- Device fingerprint age (first seen, last seen)
- Number of cards associated with this device
- Browser entropy score
- Emulator/simulator detection flag

**Geo Features (in-memory + Redis, ~1ms)**
- IP geolocation (country, city, ASN)
- Distance between IP location and billing address
- VPN/Tor/proxy detection
- Country risk tier

**Behavioral Features (Redis + Postgres, ~3ms)**
- Time since last transaction
- Amount deviation from 30-day average (z-score)
- Whether this is a new merchant for this card
- Hour-of-day transaction pattern deviation

All feature lookups run in parallel using asyncio (Python) or goroutines (Go velocity service). Total feature extraction time: 5-8ms at p99.

### Layer 2: Model Inference

Three models evaluate the feature vector in parallel:

**Rules Engine (Drools, Java)**
Handles deterministic rules that must always fire regardless of ML model output:
- Sanctioned country blocks (OFAC, EU sanctions lists)
- BIN-level blocks (compromised BIN ranges)
- Merchant-specific velocity limits
- Amount thresholds per merchant category code (MCC)
- Known fraud pattern signatures

Rules are stored in a Git repository and loaded at startup. Hot-reload is supported via a `/rules/reload` admin endpoint that triggers a Drools session rebuild without pod restart.

**XGBoost Classifier**
- 47 input features
- Trained on 18 months of labeled transaction data (~200M transactions)
- Binary classification: fraud / not-fraud
- Output: probability score [0, 1]
- Model served via a local process (not a separate service) to avoid network hop
- Retrained weekly, validated against a 3-day holdout window
- Model registry managed through MLflow

**Autoencoder (Anomaly Detection)**
- Trained exclusively on legitimate transactions
- Reconstruction error serves as anomaly score
- Catches novel fraud patterns that supervised models have not seen
- Particularly effective against account takeover and synthetic identity fraud
- Served via TensorFlow Serving (GPU instance)

### Layer 3: Score Combination and Decision

The three model outputs are combined using a weighted ensemble:

```
final_score = (w_rules * rules_score) + (w_xgb * xgb_score) + (w_ae * ae_score)
```

Default weights: rules=0.25, xgboost=0.45, autoencoder=0.30

Weights are calibrated monthly using Bayesian optimization. The objective function minimizes:
```
loss = alpha * false_positive_rate + beta * (1 - fraud_detection_rate)
```

Where alpha and beta are set by the business team based on the cost of false declines versus fraud losses.

## Feedback Loop

The fraud engine improves continuously through a closed feedback loop:

1. **Analyst Decisions:** When a fraud analyst reviews a flagged transaction, their decision (confirm fraud / clear) is fed back into the training pipeline
2. **Chargeback Data:** Chargebacks received through the settlement service are matched to original transactions and labeled as confirmed fraud
3. **Customer Disputes:** Transactions disputed by cardholders that are resolved as fraud contribute to the training set
4. **Model Retraining:** Weekly automated retraining with the updated labeled dataset
5. **Champion/Challenger:** New models are deployed as challengers (shadow scoring, no decision authority) for 48 hours before promotion

## Degradation Behavior

When the fraud engine is unavailable or slow, the payment orchestrator activates a degradation policy:

### Timeout Handling
- If the fraud engine does not respond within 50ms, the circuit breaker trips
- The orchestrator falls back to a **lightweight rule set** cached locally (updated every 5 minutes)
- This rule set covers only the highest-signal checks: sanctioned countries, known compromised BINs, extreme velocity spikes

### Merchant Tier Behavior

| Tier | Monthly Volume | Fraud Engine Down Policy |
|---|---|---|
| Tier 1 (Enterprise) | > $10M | Approve with async post-auth analysis. Enhanced monitoring for 24h |
| Tier 2 (Growth) | $500K - $10M | Approve under $500 per transaction. Queue transactions over $500 for delayed scoring |
| Tier 3 (New/High Risk) | < $500K or elevated risk | Decline all transactions. Merchant notified via webhook |

## Performance Characteristics

| Metric | Target | Achieved |
|---|---|---|
| Scoring latency (p50) | < 15ms | 11ms |
| Scoring latency (p99) | < 45ms | 38ms |
| Fraud detection rate | > 95% | 97.2% |
| False positive rate | < 1.0% | 0.73% |
| Model freshness | Weekly | Weekly (automated) |
| Rule update latency | < 5 min | ~30 seconds (hot reload) |

## Monitoring

Key dashboards for the fraud engine:

- **Score Distribution:** Real-time histogram of fraud scores. A sudden shift in distribution indicates data drift or a new attack pattern.
- **Decision Breakdown:** Approve / Decline / Step-up / Review ratios over time. Unusual spikes in any category trigger alerts.
- **Latency Heatmap:** Per-model latency breakdown to identify which component is slow.
- **Feature Availability:** Percentage of features successfully populated per request. Missing features degrade model accuracy.
- **Feedback Loop Health:** Time from analyst decision to model retraining inclusion. Target: < 7 days.
