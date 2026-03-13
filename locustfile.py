"""
Load Test Suite - Merchant Payments Platform

Simulates realistic merchant traffic patterns:
- 80% authorizations
- 10% captures (of previously authorized transactions)
- 5% refunds (of previously captured transactions)
- 5% balance/status queries

Traffic shape:
- Gradual ramp from 0 to target TPS over 5 minutes
- Sustained load for configurable duration
- Cooldown period

Usage:
  locust -f tests/load/locustfile.py --host=http://payment-gateway:8080 \
    --users 500 --spawn-rate 50 --run-time 30m
"""

import json
import random
import uuid
from datetime import datetime, timezone

from locust import HttpUser, between, task
from locust.contrib.fasthttp import FastHttpUser


# ─── Test Data Generators ───

MERCHANT_IDS = [f"mch_{uuid.uuid4().hex[:12]}" for _ in range(100)]
CURRENCIES = ["USD", "EUR", "GBP", "SAR", "AED", "INR"]
MCCS = ["5411", "5812", "5999", "7011", "4121", "5691", "5542"]
COUNTRIES = ["US", "GB", "DE", "SA", "AE", "IN", "SG"]
CHANNELS = ["ecommerce", "pos", "mobile", "recurring"]

# Amount distribution (in minor units / cents):
# 60% small (100-5000), 25% medium (5000-50000), 10% large (50000-500000), 5% very large (500000+)
AMOUNT_WEIGHTS = [(100, 5000, 0.60), (5000, 50000, 0.25), (50000, 500000, 0.10), (500000, 2000000, 0.05)]


def generate_amount():
    """Generate a realistic transaction amount based on distribution."""
    r = random.random()
    cumulative = 0
    for low, high, weight in AMOUNT_WEIGHTS:
        cumulative += weight
        if r <= cumulative:
            return random.randint(low, high)
    return random.randint(100, 5000)


def generate_card_token():
    return f"tok_{uuid.uuid4().hex[:12]}"


def generate_device_fingerprint():
    return f"fp_{uuid.uuid4().hex[:10]}"


def generate_ip():
    return f"{random.randint(1,223)}.{random.randint(0,255)}.{random.randint(0,255)}.{random.randint(1,254)}"


class PaymentUser(FastHttpUser):
    """
    Simulates a merchant integration sending payment requests.

    Each user represents a merchant's server making API calls.
    Authorized and captured payment IDs are stored for subsequent
    capture/refund operations.
    """

    wait_time = between(0.01, 0.05)  # High-frequency traffic

    def on_start(self):
        self.merchant_id = random.choice(MERCHANT_IDS)
        self.authorized_payments = []  # Queue of payment IDs available for capture
        self.captured_payments = []    # Queue of payment IDs available for refund
        self.api_key = f"sk_test_{uuid.uuid4().hex[:24]}"

    def _headers(self):
        return {
            "Content-Type": "application/json",
            "Authorization": f"Bearer {self.api_key}",
            "X-Idempotency-Key": str(uuid.uuid7() if hasattr(uuid, 'uuid7') else uuid.uuid4()),
            "X-Request-Id": str(uuid.uuid4()),
        }

    @task(80)
    def authorize_payment(self):
        """POST /v1/payments/authorize - 80% of traffic"""
        amount = generate_amount()
        currency = random.choice(CURRENCIES)
        country = random.choice(COUNTRIES)

        payload = {
            "idempotency_key": str(uuid.uuid4()),
            "merchant_id": self.merchant_id,
            "amount": {
                "value": amount,
                "currency": currency,
                "exponent": 2,
            },
            "payment_method": {
                "type": "card",
                "card": {
                    "number_token": generate_card_token(),
                    "expiry_month": random.randint(1, 12),
                    "expiry_year": random.randint(2026, 2030),
                    "cvv_provided": True,
                },
            },
            "billing_address": {
                "country": country,
                "postal_code": f"{random.randint(10000, 99999)}",
            },
            "device": {
                "fingerprint": generate_device_fingerprint(),
                "ip_address": generate_ip(),
                "user_agent": "Mozilla/5.0 (LoadTest)",
            },
            "metadata": {
                "order_id": f"ord_{uuid.uuid4().hex[:8]}",
                "channel": random.choice(CHANNELS),
            },
        }

        with self.client.post(
            "/v1/payments/authorize",
            json=payload,
            headers=self._headers(),
            name="/v1/payments/authorize",
            catch_response=True,
        ) as response:
            if response.status_code == 200:
                data = response.json()
                if data.get("status") == "authorized":
                    self.authorized_payments.append({
                        "payment_id": data["payment_id"],
                        "amount": amount,
                        "currency": currency,
                    })
                    # Keep queue bounded
                    if len(self.authorized_payments) > 100:
                        self.authorized_payments.pop(0)
                response.success()
            elif response.status_code in (400, 422):
                response.success()  # Client errors are expected (validation failures)
            else:
                response.failure(f"Unexpected status: {response.status_code}")

    @task(10)
    def capture_payment(self):
        """POST /v1/payments/{id}/capture - 10% of traffic"""
        if not self.authorized_payments:
            return

        payment = self.authorized_payments.pop(0)

        payload = {
            "amount": {
                "value": payment["amount"],
                "currency": payment["currency"],
                "exponent": 2,
            },
        }

        with self.client.post(
            f"/v1/payments/{payment['payment_id']}/capture",
            json=payload,
            headers=self._headers(),
            name="/v1/payments/{id}/capture",
            catch_response=True,
        ) as response:
            if response.status_code == 200:
                self.captured_payments.append(payment)
                if len(self.captured_payments) > 50:
                    self.captured_payments.pop(0)
                response.success()
            elif response.status_code in (400, 404, 409):
                response.success()  # Expected: already captured, expired, not found
            else:
                response.failure(f"Unexpected status: {response.status_code}")

    @task(5)
    def refund_payment(self):
        """POST /v1/payments/{id}/refund - 5% of traffic"""
        if not self.captured_payments:
            return

        payment = self.captured_payments.pop(0)

        # 70% full refund, 30% partial refund
        if random.random() < 0.3:
            refund_amount = random.randint(100, payment["amount"])
        else:
            refund_amount = payment["amount"]

        payload = {
            "idempotency_key": str(uuid.uuid4()),
            "amount": {
                "value": refund_amount,
                "currency": payment["currency"],
                "exponent": 2,
            },
            "reason": random.choice(["customer_request", "duplicate", "product_not_received"]),
        }

        with self.client.post(
            f"/v1/payments/{payment['payment_id']}/refund",
            json=payload,
            headers=self._headers(),
            name="/v1/payments/{id}/refund",
            catch_response=True,
        ) as response:
            if response.status_code in (200, 400, 404, 409):
                response.success()
            else:
                response.failure(f"Unexpected status: {response.status_code}")

    @task(5)
    def get_payment_status(self):
        """GET /v1/payments/{id} - 5% of traffic"""
        # Query a recently authorized or captured payment
        all_payments = self.authorized_payments + self.captured_payments
        if not all_payments:
            return

        payment = random.choice(all_payments)

        with self.client.get(
            f"/v1/payments/{payment['payment_id']}",
            headers=self._headers(),
            name="/v1/payments/{id}",
            catch_response=True,
        ) as response:
            if response.status_code in (200, 404):
                response.success()
            else:
                response.failure(f"Unexpected status: {response.status_code}")
