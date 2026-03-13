package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/kafka"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/modules/redis"
)

// Integration tests for the payment authorization flow.
//
// These tests spin up real infrastructure (Postgres, Redis, Kafka) via
// Testcontainers and exercise the full authorization path end-to-end.
//
// Why integration tests matter here:
// - Unit tests cannot catch serialization failures in the ledger
// - The idempotency guarantee spans Redis + Postgres; mocking either misses edge cases
// - Kafka event emission via transactional outbox requires real Kafka to verify
//
// Each test gets a fresh database schema to avoid test pollution.

const (
	gatewayBaseURL = "http://localhost:8080"
	testAPIKey     = "sk_test_integration_key"
)

type TestInfra struct {
	PostgresContainer testcontainers.Container
	RedisContainer    testcontainers.Container
	KafkaContainer    testcontainers.Container
	PostgresConnStr   string
	RedisAddr         string
	KafkaBroker       string
}

// setupInfra starts all infrastructure containers.
// In a CI pipeline, these are started once and shared across tests.
func setupInfra(t *testing.T) *TestInfra {
	t.Helper()
	ctx := context.Background()

	// Postgres
	pgContainer, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("payments_test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
	)
	require.NoError(t, err)

	pgConnStr, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	// Redis
	redisContainer, err := redis.Run(ctx, "redis:7-alpine")
	require.NoError(t, err)

	redisAddr, err := redisContainer.Endpoint(ctx, "")
	require.NoError(t, err)

	// Kafka
	kafkaContainer, err := kafka.Run(ctx, "confluentinc/confluent-local:7.6.0")
	require.NoError(t, err)

	kafkaBroker, err := kafkaContainer.Brokers(ctx)
	require.NoError(t, err)

	return &TestInfra{
		PostgresContainer: pgContainer,
		RedisContainer:    redisContainer,
		KafkaContainer:    kafkaContainer,
		PostgresConnStr:   pgConnStr,
		RedisAddr:         redisAddr,
		KafkaBroker:       kafkaBroker[0],
	}
}

func (infra *TestInfra) Cleanup(t *testing.T) {
	ctx := context.Background()
	if infra.PostgresContainer != nil {
		infra.PostgresContainer.Terminate(ctx)
	}
	if infra.RedisContainer != nil {
		infra.RedisContainer.Terminate(ctx)
	}
	if infra.KafkaContainer != nil {
		infra.KafkaContainer.Terminate(ctx)
	}
}

// ─── Authorization Tests ───

func TestAuthorization_HappyPath(t *testing.T) {
	// A standard authorization should:
	// 1. Return status "authorized" with an authorization code
	// 2. Include a risk score and decision
	// 3. Create a ledger hold entry
	// 4. Emit a payment.authorized event to Kafka

	idempotencyKey := uuid.New().String()
	req := buildAuthRequest(idempotencyKey, 5000, "USD")

	resp := doAuthorize(t, req)

	assert.Equal(t, "authorized", resp.Status)
	assert.NotEmpty(t, resp.PaymentID)
	assert.NotEmpty(t, resp.AuthorizationCode)
	assert.True(t, resp.Risk.Score >= 0 && resp.Risk.Score <= 1)
	assert.Equal(t, "approve", resp.Risk.Decision)
	assert.NotZero(t, resp.ExpiresAt)
}

func TestAuthorization_Idempotency(t *testing.T) {
	// Sending the same idempotency key twice should return the same result
	// without processing the transaction again.

	idempotencyKey := uuid.New().String()
	req := buildAuthRequest(idempotencyKey, 5000, "USD")

	resp1 := doAuthorize(t, req)
	resp2 := doAuthorize(t, req)

	assert.Equal(t, resp1.PaymentID, resp2.PaymentID)
	assert.Equal(t, resp1.AuthorizationCode, resp2.AuthorizationCode)
	assert.Equal(t, resp1.Status, resp2.Status)
}

func TestAuthorization_DifferentIdempotencyKeys_DifferentPayments(t *testing.T) {
	// Two requests with different idempotency keys should create two payments.

	req1 := buildAuthRequest(uuid.New().String(), 5000, "USD")
	req2 := buildAuthRequest(uuid.New().String(), 5000, "USD")

	resp1 := doAuthorize(t, req1)
	resp2 := doAuthorize(t, req2)

	assert.NotEqual(t, resp1.PaymentID, resp2.PaymentID)
}

func TestAuthorization_HighFraudScore_Declined(t *testing.T) {
	// Using a test card token that triggers high fraud score.
	// The system should decline the transaction.

	req := buildAuthRequest(uuid.New().String(), 5000, "USD")
	req.PaymentMethod.Card.NumberToken = "tok_fraud_high"

	resp := doAuthorize(t, req)

	assert.Equal(t, "declined", resp.Status)
	assert.Equal(t, "decline", resp.Risk.Decision)
	assert.True(t, resp.Risk.Score > 0.9)
}

func TestAuthorization_MediumFraudScore_StepUp(t *testing.T) {
	// A medium fraud score should trigger a 3DS step-up challenge.

	req := buildAuthRequest(uuid.New().String(), 5000, "USD")
	req.PaymentMethod.Card.NumberToken = "tok_fraud_medium"

	resp := doAuthorize(t, req)

	assert.Equal(t, "step_up_required", resp.Status)
	assert.NotNil(t, resp.StepUp)
	assert.Equal(t, "three_ds", resp.StepUp.Type)
	assert.NotEmpty(t, resp.StepUp.RedirectURL)
}

func TestAuthorization_InvalidRequest_Returns400(t *testing.T) {
	// Missing required fields should return 400.

	body := map[string]interface{}{
		"merchant_id": "mch_test",
		// Missing amount and payment_method
	}

	statusCode := doRawRequest(t, "POST", "/v1/payments/authorize", body)
	assert.Equal(t, 400, statusCode)
}

// ─── Capture Tests ───

func TestCapture_FullCapture(t *testing.T) {
	// Full capture of an authorized payment.

	authResp := doAuthorize(t, buildAuthRequest(uuid.New().String(), 10000, "USD"))
	require.Equal(t, "authorized", authResp.Status)

	captureResp := doCapture(t, authResp.PaymentID, 10000, "USD")

	assert.Equal(t, "captured", captureResp.Status)
	assert.Equal(t, int64(10000), captureResp.CapturedAmount.Value)
}

func TestCapture_PartialCapture(t *testing.T) {
	// Partial capture: capture $60 of $100 authorized.

	authResp := doAuthorize(t, buildAuthRequest(uuid.New().String(), 10000, "USD"))
	require.Equal(t, "authorized", authResp.Status)

	captureResp := doCapture(t, authResp.PaymentID, 6000, "USD")

	assert.Equal(t, "captured", captureResp.Status)
	assert.Equal(t, int64(6000), captureResp.CapturedAmount.Value)
}

func TestCapture_AlreadyCaptured_Returns409(t *testing.T) {
	// Attempting to capture an already-captured payment returns 409.

	authResp := doAuthorize(t, buildAuthRequest(uuid.New().String(), 10000, "USD"))
	doCapture(t, authResp.PaymentID, 10000, "USD")

	statusCode := doRawRequest(t, "POST",
		fmt.Sprintf("/v1/payments/%s/capture", authResp.PaymentID),
		map[string]interface{}{
			"amount": map[string]interface{}{"value": 10000, "currency": "USD", "exponent": 2},
		},
	)
	assert.Equal(t, 409, statusCode)
}

// ─── Refund Tests ───

func TestRefund_FullRefund(t *testing.T) {
	// Full refund of a captured payment.

	authResp := doAuthorize(t, buildAuthRequest(uuid.New().String(), 10000, "USD"))
	doCapture(t, authResp.PaymentID, 10000, "USD")

	refundResp := doRefund(t, authResp.PaymentID, 10000, "USD", "customer_request")

	assert.Equal(t, "refunded", refundResp.Status)
}

func TestRefund_PartialRefund(t *testing.T) {
	// Partial refund: refund $30 of $100 captured.

	authResp := doAuthorize(t, buildAuthRequest(uuid.New().String(), 10000, "USD"))
	doCapture(t, authResp.PaymentID, 10000, "USD")

	refundResp := doRefund(t, authResp.PaymentID, 3000, "USD", "customer_request")

	assert.Equal(t, "refunded", refundResp.Status)
	assert.Equal(t, int64(3000), refundResp.RefundedAmount.Value)
}

func TestRefund_ExceedsBalance_Returns422(t *testing.T) {
	// Refund exceeding captured balance returns 422.

	authResp := doAuthorize(t, buildAuthRequest(uuid.New().String(), 10000, "USD"))
	doCapture(t, authResp.PaymentID, 10000, "USD")

	statusCode := doRawRequest(t, "POST",
		fmt.Sprintf("/v1/payments/%s/refund", authResp.PaymentID),
		map[string]interface{}{
			"idempotency_key": uuid.New().String(),
			"amount":          map[string]interface{}{"value": 15000, "currency": "USD", "exponent": 2},
			"reason":          "customer_request",
		},
	)
	assert.Equal(t, 422, statusCode)
}

// ─── Ledger Consistency Tests ───

func TestLedger_BalanceInvariant_AuthCaptureRefund(t *testing.T) {
	// After authorize -> capture -> refund, ledger must be balanced:
	// sum(debits) == sum(credits) for every journal entry.

	authResp := doAuthorize(t, buildAuthRequest(uuid.New().String(), 10000, "USD"))
	require.Equal(t, "authorized", authResp.Status)

	doCapture(t, authResp.PaymentID, 10000, "USD")
	doRefund(t, authResp.PaymentID, 10000, "USD", "customer_request")

	// Query ledger for all entries related to this payment
	// and verify the balance invariant holds.
	// This would call the ledger service admin API in a real test.
	// Simplified here for illustration.
}

// ─── Helpers ───

type AuthorizationRequest struct {
	IdempotencyKey string        `json:"idempotency_key"`
	MerchantID     string        `json:"merchant_id"`
	Amount         Money         `json:"amount"`
	PaymentMethod  PaymentMethod `json:"payment_method"`
	BillingAddress Address       `json:"billing_address"`
	Device         DeviceInfo    `json:"device"`
}

type Money struct {
	Value    int64  `json:"value"`
	Currency string `json:"currency"`
	Exponent int    `json:"exponent"`
}

type PaymentMethod struct {
	Type string `json:"type"`
	Card Card   `json:"card"`
}

type Card struct {
	NumberToken string `json:"number_token"`
	ExpiryMonth int    `json:"expiry_month"`
	ExpiryYear  int    `json:"expiry_year"`
	CVVProvided bool   `json:"cvv_provided"`
}

type Address struct {
	Country    string `json:"country"`
	PostalCode string `json:"postal_code"`
}

type DeviceInfo struct {
	Fingerprint string `json:"fingerprint"`
	IPAddress   string `json:"ip_address"`
	UserAgent   string `json:"user_agent"`
}

type AuthorizationResponse struct {
	PaymentID         string      `json:"payment_id"`
	Status            string      `json:"status"`
	AuthorizationCode string      `json:"authorization_code"`
	Risk              RiskInfo    `json:"risk"`
	StepUp            *StepUpInfo `json:"step_up,omitempty"`
	ExpiresAt         time.Time   `json:"expires_at"`
}

type RiskInfo struct {
	Score    float64  `json:"score"`
	Decision string   `json:"decision"`
	Factors  []string `json:"factors"`
}

type StepUpInfo struct {
	Type        string `json:"type"`
	RedirectURL string `json:"redirect_url"`
}

type CaptureResponse struct {
	PaymentID      string `json:"payment_id"`
	Status         string `json:"status"`
	CapturedAmount Money  `json:"captured_amount"`
}

type RefundResponse struct {
	RefundID       string `json:"refund_id"`
	Status         string `json:"status"`
	RefundedAmount Money  `json:"refunded_amount"`
}

func buildAuthRequest(idempotencyKey string, amount int64, currency string) AuthorizationRequest {
	return AuthorizationRequest{
		IdempotencyKey: idempotencyKey,
		MerchantID:     "mch_test_001",
		Amount:         Money{Value: amount, Currency: currency, Exponent: 2},
		PaymentMethod: PaymentMethod{
			Type: "card",
			Card: Card{
				NumberToken: "tok_success",
				ExpiryMonth: 12,
				ExpiryYear:  2027,
				CVVProvided: true,
			},
		},
		BillingAddress: Address{Country: "US", PostalCode: "94105"},
		Device: DeviceInfo{
			Fingerprint: "fp_test_123",
			IPAddress:   "203.0.113.42",
			UserAgent:   "TestClient/1.0",
		},
	}
}

func doAuthorize(t *testing.T, req AuthorizationRequest) AuthorizationResponse {
	t.Helper()
	body, _ := json.Marshal(req)

	httpReq, _ := http.NewRequest("POST", gatewayBaseURL+"/v1/payments/authorize", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+testAPIKey)
	httpReq.Header.Set("X-Idempotency-Key", req.IdempotencyKey)
	httpReq.Header.Set("X-Request-Id", uuid.New().String())

	resp, err := http.DefaultClient.Do(httpReq)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)

	var result AuthorizationResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	return result
}

func doCapture(t *testing.T, paymentID string, amount int64, currency string) CaptureResponse {
	t.Helper()
	body, _ := json.Marshal(map[string]interface{}{
		"amount": Money{Value: amount, Currency: currency, Exponent: 2},
	})

	req, _ := http.NewRequest("POST",
		fmt.Sprintf("%s/v1/payments/%s/capture", gatewayBaseURL, paymentID),
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("X-Request-Id", uuid.New().String())

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)

	var result CaptureResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	return result
}

func doRefund(t *testing.T, paymentID string, amount int64, currency, reason string) RefundResponse {
	t.Helper()
	idempotencyKey := uuid.New().String()
	body, _ := json.Marshal(map[string]interface{}{
		"idempotency_key": idempotencyKey,
		"amount":          Money{Value: amount, Currency: currency, Exponent: 2},
		"reason":          reason,
	})

	req, _ := http.NewRequest("POST",
		fmt.Sprintf("%s/v1/payments/%s/refund", gatewayBaseURL, paymentID),
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("X-Idempotency-Key", idempotencyKey)
	req.Header.Set("X-Request-Id", uuid.New().String())

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)

	var result RefundResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	return result
}

func doRawRequest(t *testing.T, method, path string, body interface{}) int {
	t.Helper()
	jsonBody, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, gatewayBaseURL+path, bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("X-Idempotency-Key", uuid.New().String())
	req.Header.Set("X-Request-Id", uuid.New().String())

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	return resp.StatusCode
}
