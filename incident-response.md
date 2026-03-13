# Incident Response Runbook

## Severity Classification

| Severity | Definition | Response SLA | Examples |
|---|---|---|---|
| P0 | Revenue-impacting, customer-facing service down | 5 min acknowledge, 15 min mitigation | Auth success rate < 85%, Ledger imbalance, Full service outage |
| P1 | Degraded service, partial impact | 15 min acknowledge, 1h mitigation | Fraud engine down (degradation active), Kafka lag > 10K, Latency 2x normal |
| P2 | Non-critical degradation, no immediate customer impact | 1h acknowledge, 4h mitigation | Settlement batch delayed, Redis hit rate drop, Non-critical service restart |
| P3 | Informational, proactive | Next business day | Disk usage trending up, Certificate renewal needed, Dependency deprecation |

## P0: Authorization Success Rate Drop

### Symptoms
- Grafana alert: `payment_auth_success_rate < 0.85`
- Merchant complaints about transaction declines
- PagerDuty page to on-call engineer

### Diagnosis

**Step 1: Identify scope**
```bash
# Check if the drop is global or merchant-specific
curl -s "http://prometheus:9090/api/v1/query?query=rate(payment_authorization_total{status='success'}[5m])/rate(payment_authorization_total[5m])" | jq

# Check per-service error rates
curl -s "http://prometheus:9090/api/v1/query?query=rate(grpc_server_handled_total{grpc_code!='OK'}[5m])" | jq
```

**Step 2: Check downstream dependencies**
```bash
# Card network connectivity
kubectl exec -n payments deploy/payment-gateway -- curl -s http://localhost:8081/health/dependencies | jq

# Fraud engine health
kubectl exec -n payments deploy/fraud-engine -- curl -s http://localhost:8081/health | jq

# Ledger service health
kubectl exec -n payments deploy/ledger-service -- curl -s http://localhost:8081/health | jq

# Database connectivity
kubectl exec -n payments deploy/payment-gateway -- curl -s http://localhost:8081/health/db | jq
```

**Step 3: Check for deployment correlation**
```bash
# Recent deployments
kubectl -n payments get events --sort-by='.lastTimestamp' | head -20

# Recent pod restarts
kubectl -n payments get pods --sort-by='.status.containerStatuses[0].restartCount' | tail -10
```

### Common Causes and Mitigations

**Card network unreachable:**
- Check VPN tunnel status to Visa/Mastercard
- Verify TLS certificate validity
- Failover to backup network endpoint if available
- Escalate to network operations team

**Fraud engine overloaded:**
- Check if fraud engine circuit breaker is open
- If yes: degradation policy is active, auth continues with reduced fraud coverage
- Scale fraud engine pods: `kubectl -n payments scale deploy/fraud-engine --replicas=12`
- Check ML model serving latency: may need GPU instance restart

**Database connection pool exhausted:**
- Check PgBouncer stats: `kubectl exec -n payments deploy/pgbouncer -- pgbouncer -d payments_pgbouncer -c "SHOW POOLS;"`
- Check for long-running queries: query `pg_stat_activity`
- If caused by serialization failures: check for hot-row contention in ledger
- Emergency: increase connection pool size via config reload

**Recent deployment caused regression:**
- Identify the deployment: `kubectl -n payments rollout history deploy/payment-gateway`
- Rollback: `kubectl -n payments rollout undo deploy/payment-gateway`
- Blue/green switch if using blue/green strategy

### Escalation
- If not mitigated within 15 minutes: page engineering manager
- If not mitigated within 30 minutes: page VP Engineering
- If customer-reported: notify merchant operations team immediately

---

## P0: Ledger Imbalance Detected

### Symptoms
- Alert: `ledger_balance_invariant_violation`
- Reconciliation engine detected sum(debits) != sum(credits) for one or more journal entries

### THIS IS THE MOST CRITICAL ALERT IN THE SYSTEM

A ledger imbalance means money accounting is incorrect. This could indicate:
1. A bug in the ledger write path
2. A partial write (transaction committed but not all journal lines persisted)
3. Data corruption

### Immediate Actions

**Step 1: Do NOT attempt to fix the data manually.** All fixes go through the standard correction entry process.

**Step 2: Identify the scope**
```sql
-- Find imbalanced entries
SELECT
    je.entry_id,
    je.entry_type,
    je.transaction_id,
    je.created_at,
    SUM(CASE WHEN jl.direction = 'DEBIT' THEN jl.amount ELSE 0 END) AS total_debits,
    SUM(CASE WHEN jl.direction = 'CREDIT' THEN jl.amount ELSE 0 END) AS total_credits,
    SUM(CASE WHEN jl.direction = 'DEBIT' THEN jl.amount ELSE 0 END) -
    SUM(CASE WHEN jl.direction = 'CREDIT' THEN jl.amount ELSE 0 END) AS imbalance
FROM journal_entries je
JOIN journal_lines jl ON je.entry_id = jl.entry_id
GROUP BY je.entry_id, je.entry_type, je.transaction_id, je.created_at
HAVING SUM(CASE WHEN jl.direction = 'DEBIT' THEN jl.amount ELSE 0 END) !=
       SUM(CASE WHEN jl.direction = 'CREDIT' THEN jl.amount ELSE 0 END)
ORDER BY je.created_at DESC;
```

**Step 3: Check the originating event**
```bash
# Find the Kafka event that triggered the imbalanced entry
kafka-console-consumer --bootstrap-server $KAFKA_BROKERS \
  --topic ledger.entries \
  --from-beginning \
  --max-messages 100 \
  --property print.key=true | grep $ENTRY_ID
```

**Step 4: Create a correcting entry**
Correcting entries are created through the admin API, never through direct database access:
```bash
curl -X POST http://ledger-service:8080/admin/correction \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "original_entry_id": "<entry_id>",
    "reason": "Ledger imbalance correction - incident INC-XXXX",
    "correction_lines": [
      {"account_id": "...", "direction": "CREDIT", "amount": 100, "currency": "USD"}
    ],
    "approved_by": "<engineer_name>",
    "incident_id": "INC-XXXX"
  }'
```

### Post-Incident
- Root cause analysis required within 48 hours
- All ledger imbalance incidents require a written post-mortem
- If the imbalance affected merchant balances: merchant operations must be notified

---

## P1: Fraud Engine Unavailable

### Symptoms
- Alert: `fraud_engine_circuit_breaker_open`
- Fraud scoring latency p99 > 100ms
- Degradation policy activated for merchant tiers

### Impact
Authorization continues, but with reduced fraud protection. Merchant-tier degradation policies are in effect:
- Tier 1: Approved with post-auth monitoring
- Tier 2: Approved under threshold, queued above
- Tier 3: All declined

### Diagnosis
```bash
# Check fraud engine pod status
kubectl -n payments get pods -l app=fraud-engine

# Check fraud engine logs for errors
kubectl -n payments logs -l app=fraud-engine --tail=100 | grep ERROR

# Check GPU utilization (if ML inference is slow)
kubectl -n payments exec deploy/fraud-engine -- nvidia-smi

# Check Redis (feature store) connectivity
kubectl -n payments exec deploy/fraud-engine -- python -c "import redis; r = redis.Redis('redis-cluster'); print(r.ping())"

# Check model loading status
kubectl -n payments exec deploy/fraud-engine -- curl -s http://localhost:8081/models/status | jq
```

### Mitigation
1. If pod crash loop: check logs, restart if OOM, scale up memory limits
2. If GPU issue: restart pod on different node, check GPU driver compatibility
3. If Redis down: fraud engine should fall back to local cache (5s stale data acceptable)
4. If model loading failed: rollback to previous model version via MLflow

```bash
# Rollback fraud model
kubectl -n payments set env deploy/fraud-engine FRAUD_MODEL_VERSION=v47  # previous stable version
```

---

## P1: Kafka Consumer Lag

### Symptoms
- Alert: `kafka_consumer_lag > 10000` for any critical consumer group
- Settlement or notification delays

### Diagnosis
```bash
# Check consumer group lag
kafka-consumer-groups --bootstrap-server $KAFKA_BROKERS \
  --group payment-ledger-consumer \
  --describe

# Check broker health
kafka-broker-api-versions --bootstrap-server $KAFKA_BROKERS

# Check for slow consumers
kubectl -n payments logs -l app=ledger-service --tail=50 | grep -i "processing time"
```

### Mitigation
1. Scale consumer instances if processing is CPU-bound
2. Check for poison pill messages (malformed events blocking consumer)
3. If lag is on settlement topics and outside business hours: may be acceptable, monitor
4. If lag is on ledger topics: urgent, balance computations will be stale

---

## P2: Settlement Batch Failure

### Symptoms
- Alert: `settlement_batch_status = FAILED`
- Clearing file rejected by card network
- Merchant payout delayed

### Diagnosis
```bash
# Check settlement batch status
curl -s http://settlement-service:8080/admin/batches/latest | jq

# Check clearing file generation logs
kubectl -n payments logs -l app=settlement-service --tail=200 | grep -E "clearing|batch|error"

# Check network connectivity to Visa/MC SFTP
kubectl -n payments exec deploy/settlement-service -- sftp -o BatchMode=yes visa-clearing@sftp.visa.com
```

### Mitigation
1. Parse network rejection response to identify failed transactions
2. Fix data issues in failed transactions
3. Resubmit failed transactions in next clearing batch
4. If systemic issue: escalate to card network operations

---

## General Procedures

### On-Call Rotation
- Primary on-call: Payments engineering team (weekly rotation)
- Secondary on-call: Platform engineering team
- Escalation: Engineering Manager -> VP Engineering -> CTO

### Communication During Incidents
- P0/P1: Open incident channel in Slack (#incident-payments-YYYYMMDD)
- Post updates every 15 minutes during active incident
- Notify merchant operations for any customer-impacting issues
- Post-incident review within 48 hours for P0, 1 week for P1

### Useful Links
- Grafana Dashboards: https://grafana.internal/d/payments-overview
- PagerDuty: https://pagerduty.com/services/payments
- Runbook Index: https://wiki.internal/payments/runbooks
- Architecture Docs: /docs/architecture/
