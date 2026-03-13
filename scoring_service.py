"""
Fraud Scoring Service

Real-time fraud scoring pipeline that combines rule engine, ML models, and velocity checks
to produce a risk score and decision within 45ms at p99.

Architecture:
- Feature extraction runs in parallel (asyncio gather)
- Three models score independently, results are combined via weighted ensemble
- Decision engine maps final score to action (approve/decline/step_up/review)
- All decisions are logged for audit trail and model feedback loop
"""

import asyncio
import logging
import time
from dataclasses import dataclass
from enum import Enum
from typing import Optional
from uuid import UUID

import numpy as np
from prometheus_client import Counter, Histogram

logger = logging.getLogger(__name__)

# ─── Metrics ───

SCORING_LATENCY = Histogram(
    "fraud_scoring_duration_seconds",
    "Fraud scoring latency",
    ["model"],
    buckets=[0.005, 0.010, 0.015, 0.025, 0.035, 0.045, 0.060, 0.100],
)
SCORING_DECISIONS = Counter(
    "fraud_scoring_decisions_total",
    "Fraud scoring decisions",
    ["decision"],
)
FEATURE_EXTRACTION_ERRORS = Counter(
    "fraud_feature_extraction_errors_total",
    "Feature extraction failures",
    ["feature_group"],
)


class Decision(str, Enum):
    APPROVE = "approve"
    APPROVE_FLAG = "approve_flag"
    STEP_UP = "step_up"
    REVIEW = "review"
    DECLINE = "decline"


@dataclass(frozen=True)
class FraudScore:
    transaction_id: UUID
    final_score: float
    decision: Decision
    rules_score: float
    xgboost_score: float
    anomaly_score: float
    risk_factors: list[str]
    latency_ms: float
    features_used: int
    features_available: int


@dataclass(frozen=True)
class TransactionContext:
    """All signals needed for fraud scoring, assembled by the payment gateway."""
    transaction_id: UUID
    merchant_id: str
    card_token: str
    amount_minor: int
    currency: str
    mcc: str
    ip_address: str
    device_fingerprint: Optional[str]
    billing_country: str
    shipping_country: Optional[str]
    user_agent: Optional[str]
    is_3ds_authenticated: bool
    channel: str  # ecommerce, pos, mobile, recurring


class FraudScoringService:
    """
    Orchestrates the fraud scoring pipeline.

    The scoring flow:
    1. Extract features from multiple stores in parallel
    2. Run three models in parallel (rules, xgboost, autoencoder)
    3. Combine scores using weighted ensemble
    4. Map combined score to a decision
    5. Return score + decision + risk factors

    Timeout budget: 45ms total at p99
    - Feature extraction: 8ms
    - Model inference: 25ms (parallel, slowest model dominates)
    - Score combination + decision: 2ms
    - Overhead: 10ms buffer
    """

    def __init__(
        self,
        velocity_store,
        device_store,
        geo_service,
        behavioral_store,
        account_store,
        rules_engine,
        xgboost_model,
        anomaly_model,
        config,
    ):
        self.velocity_store = velocity_store
        self.device_store = device_store
        self.geo_service = geo_service
        self.behavioral_store = behavioral_store
        self.account_store = account_store
        self.rules_engine = rules_engine
        self.xgboost_model = xgboost_model
        self.anomaly_model = anomaly_model
        self.config = config

    async def score(self, txn: TransactionContext) -> FraudScore:
        start = time.monotonic()

        # ─── Step 1: Parallel Feature Extraction ───
        features, extraction_stats = await self._extract_features(txn)

        # ─── Step 2: Parallel Model Scoring ───
        rules_result, xgb_result, anomaly_result = await asyncio.gather(
            self._score_rules(txn, features),
            self._score_xgboost(features),
            self._score_anomaly(features),
        )

        # ─── Step 3: Score Combination ───
        weights = self.config.get_model_weights(txn.merchant_id)
        final_score = (
            weights.rules * rules_result.score
            + weights.xgboost * xgb_result.score
            + weights.anomaly * anomaly_result.score
        )
        final_score = np.clip(final_score, 0.0, 1.0)

        # ─── Step 4: Decision ───
        thresholds = self.config.get_thresholds(txn.merchant_id, txn.mcc)
        decision = self._make_decision(final_score, thresholds)

        # Collect risk factors from all models
        risk_factors = (
            rules_result.factors + xgb_result.top_factors + anomaly_result.factors
        )

        elapsed_ms = (time.monotonic() - start) * 1000

        # ─── Metrics ───
        SCORING_LATENCY.labels(model="total").observe(elapsed_ms / 1000)
        SCORING_DECISIONS.labels(decision=decision.value).increment()

        result = FraudScore(
            transaction_id=txn.transaction_id,
            final_score=round(final_score, 4),
            decision=decision,
            rules_score=round(rules_result.score, 4),
            xgboost_score=round(xgb_result.score, 4),
            anomaly_score=round(anomaly_result.score, 4),
            risk_factors=risk_factors[:10],  # Top 10 factors
            latency_ms=round(elapsed_ms, 2),
            features_used=extraction_stats.features_populated,
            features_available=extraction_stats.features_total,
        )

        logger.info(
            "Fraud scoring complete. txn=%s score=%.4f decision=%s latency=%.2fms features=%d/%d",
            txn.transaction_id,
            result.final_score,
            result.decision.value,
            result.latency_ms,
            result.features_used,
            result.features_available,
        )

        return result

    async def _extract_features(self, txn: TransactionContext):
        """
        Extract all feature groups in parallel.

        Each feature group is wrapped in a try/except so a failure in one group
        (e.g., geo lookup timeout) does not block the entire scoring pipeline.
        Missing features are filled with defaults, and the model handles them
        via missing value imputation (XGBoost native) or default anomaly thresholds.
        """
        results = await asyncio.gather(
            self._safe_extract("velocity", self.velocity_store.get_features, txn.card_token),
            self._safe_extract("device", self.device_store.get_features, txn.device_fingerprint),
            self._safe_extract("geo", self.geo_service.get_features, txn.ip_address),
            self._safe_extract("behavioral", self.behavioral_store.get_features, txn.card_token),
            self._safe_extract("account", self.account_store.get_features, txn.card_token),
            return_exceptions=True,
        )

        combined = FeatureVector()
        populated = 0
        total = 0

        for group_name, features in zip(
            ["velocity", "device", "geo", "behavioral", "account"], results
        ):
            if isinstance(features, Exception):
                logger.warning(
                    "Feature group %s failed for txn %s: %s",
                    group_name,
                    txn.transaction_id,
                    features,
                )
                FEATURE_EXTRACTION_ERRORS.labels(feature_group=group_name).increment()
                combined.merge_defaults(group_name)
            else:
                combined.merge(features)
                populated += features.count_populated()
            total += combined.count_for_group(group_name)

        # Add transaction-level features (always available)
        combined.set("txn_amount_minor", txn.amount_minor)
        combined.set("txn_currency", txn.currency)
        combined.set("txn_mcc", txn.mcc)
        combined.set("txn_channel", txn.channel)
        combined.set("txn_is_3ds", 1 if txn.is_3ds_authenticated else 0)
        combined.set("txn_billing_country", txn.billing_country)
        combined.set(
            "txn_cross_border",
            1 if txn.shipping_country and txn.shipping_country != txn.billing_country else 0,
        )
        populated += 7
        total += 7

        stats = ExtractionStats(features_populated=populated, features_total=total)
        return combined, stats

    async def _safe_extract(self, group_name, extractor_fn, key):
        """Wrap feature extraction with timeout and error handling."""
        try:
            return await asyncio.wait_for(extractor_fn(key), timeout=0.008)
        except asyncio.TimeoutError:
            logger.warning("Feature group %s timed out for key %s", group_name, key)
            raise
        except Exception as e:
            logger.warning("Feature group %s failed for key %s: %s", group_name, key, e)
            raise

    async def _score_rules(self, txn, features):
        """
        Evaluate deterministic rules. Rules always run, even when ML models are degraded.

        Rule categories:
        - Block rules: Sanctioned countries, compromised BINs (instant decline)
        - Velocity rules: Transaction count/amount thresholds per time window
        - Pattern rules: Known fraud signatures (e.g., card testing patterns)
        - Merchant rules: Per-merchant custom thresholds
        """
        with SCORING_LATENCY.labels(model="rules").time():
            return await self.rules_engine.evaluate(txn, features)

    async def _score_xgboost(self, features):
        """
        Run XGBoost classifier on the feature vector.

        Model details:
        - 47 input features
        - Trained on ~200M labeled transactions
        - Binary output: P(fraud)
        - Handles missing features natively (XGBoost learn branch)
        """
        with SCORING_LATENCY.labels(model="xgboost").time():
            vector = features.to_numpy_array()
            return await self.xgboost_model.predict(vector)

    async def _score_anomaly(self, features):
        """
        Run autoencoder anomaly detection.

        The model is trained on legitimate transactions only. High reconstruction
        error indicates the transaction deviates from normal patterns, which may
        signal novel fraud that supervised models have not seen.
        """
        with SCORING_LATENCY.labels(model="anomaly").time():
            vector = features.to_numpy_array(normalize=True)
            return await self.anomaly_model.predict(vector)

    def _make_decision(self, score: float, thresholds) -> Decision:
        """
        Map combined fraud score to a decision.

        Thresholds are configurable per merchant and per MCC. The default thresholds:
          0.00 - 0.30  APPROVE
          0.30 - 0.55  APPROVE_FLAG  (approve, queue for post-auth review)
          0.55 - 0.75  STEP_UP       (trigger 3DS/OTP challenge)
          0.75 - 0.90  REVIEW        (hold for manual review, 15 min SLA)
          0.90 - 1.00  DECLINE
        """
        if score >= thresholds.decline:
            return Decision.DECLINE
        elif score >= thresholds.review:
            return Decision.REVIEW
        elif score >= thresholds.step_up:
            return Decision.STEP_UP
        elif score >= thresholds.flag:
            return Decision.APPROVE_FLAG
        else:
            return Decision.APPROVE


# ─── Supporting types (simplified for readability) ───


@dataclass
class FeatureVector:
    _data: dict = None

    def __post_init__(self):
        if self._data is None:
            self._data = {}

    def set(self, key: str, value):
        self._data[key] = value

    def merge(self, other):
        self._data.update(other._data)

    def merge_defaults(self, group_name: str):
        """Fill missing feature group with default values (NaN for numeric)."""
        defaults = FEATURE_DEFAULTS.get(group_name, {})
        for key, default_val in defaults.items():
            if key not in self._data:
                self._data[key] = default_val

    def count_populated(self) -> int:
        return sum(1 for v in self._data.values() if v is not None and v is not np.nan)

    def count_for_group(self, group_name: str) -> int:
        return len(FEATURE_DEFAULTS.get(group_name, {}))

    def to_numpy_array(self, normalize: bool = False) -> np.ndarray:
        ordered = [self._data.get(f, np.nan) for f in FEATURE_ORDER]
        arr = np.array(ordered, dtype=np.float64)
        if normalize:
            arr = (arr - FEATURE_MEANS) / FEATURE_STDS
        return arr


@dataclass
class ExtractionStats:
    features_populated: int
    features_total: int


# Feature order and defaults would be loaded from config in production.
# Shown here as constants for clarity.
FEATURE_ORDER = [
    "vel_tx_count_1h", "vel_tx_count_6h", "vel_tx_count_24h",
    "vel_amount_sum_1h", "vel_amount_sum_24h", "vel_unique_merchants_24h",
    "vel_decline_rate_7d",
    "dev_fingerprint_age_days", "dev_cards_on_device", "dev_browser_entropy",
    "dev_is_emulator",
    "geo_ip_country_risk", "geo_billing_ship_distance_km", "geo_is_vpn",
    "geo_is_tor",
    "beh_time_since_last_tx_sec", "beh_amount_z_score", "beh_new_merchant",
    "beh_avg_amount_30d",
    "acc_age_days", "acc_lifetime_tx_count", "acc_chargeback_rate",
    "acc_historical_fraud",
    "txn_amount_minor", "txn_mcc", "txn_channel", "txn_is_3ds",
    "txn_cross_border",
]

FEATURE_DEFAULTS = {
    "velocity": {f: np.nan for f in FEATURE_ORDER if f.startswith("vel_")},
    "device": {f: np.nan for f in FEATURE_ORDER if f.startswith("dev_")},
    "geo": {f: np.nan for f in FEATURE_ORDER if f.startswith("geo_")},
    "behavioral": {f: np.nan for f in FEATURE_ORDER if f.startswith("beh_")},
    "account": {f: np.nan for f in FEATURE_ORDER if f.startswith("acc_")},
}

# Normalization constants (computed from training data, loaded from config in production)
FEATURE_MEANS = np.zeros(len(FEATURE_ORDER))
FEATURE_STDS = np.ones(len(FEATURE_ORDER))
