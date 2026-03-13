package settlement

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
)

// Service handles the settlement lifecycle: clearing file generation,
// net settlement computation, and merchant payout scheduling.
//
// Why Go for settlement:
// - Batch-oriented workload: settlement runs on a schedule, not on the hot path
// - Excellent concurrency for parallel clearing file generation per network
// - Low memory footprint for processing large transaction batches
// - Strong standard library for file I/O and SFTP operations
//
// Settlement cycle:
// T+0: Transactions captured, ledger entries created
// T+1: Clearing files generated and submitted to card networks
// T+2: Network confirms clearing, net settlement computed
// T+3: Merchant payout disbursed

var (
	batchesCreated = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "settlement_batches_created_total",
			Help: "Settlement batches created",
		},
		[]string{"network", "status"},
	)
	batchDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "settlement_batch_duration_seconds",
			Help:    "Time to process a settlement batch",
			Buckets: []float64{1, 5, 10, 30, 60, 120, 300},
		},
		[]string{"network"},
	)
	lastSuccessfulBatch = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "settlement_last_successful_batch_timestamp",
		Help: "Unix timestamp of last successful settlement batch",
	})
	payoutAmount = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "settlement_payout_amount_total",
			Help: "Total payout amount in minor units",
		},
		[]string{"currency"},
	)
)

func init() {
	prometheus.MustRegister(batchesCreated, batchDuration, lastSuccessfulBatch, payoutAmount)
}

// Service orchestrates the settlement process.
type Service struct {
	db              *sql.DB
	visaClient      ClearingClient
	mastercardClient ClearingClient
	bankingClient   PayoutClient
	eventPublisher  EventPublisher
	config          Config
	logger          *slog.Logger
}

// Config holds settlement configuration.
type Config struct {
	DefaultSettlementCycle int           // Days from capture to payout (default: 3)
	ClearingCutoffHour     int           // UTC hour for daily clearing cutoff (default: 23)
	MinPayoutThreshold     map[string]int64 // Minimum payout per currency in minor units
	MaxBatchSize           int           // Max transactions per clearing file
}

// Batch represents a settlement batch for a specific network and date.
type Batch struct {
	BatchID       uuid.UUID
	Network       string
	SettlementDate time.Time
	Status        string
	TransactionCount int
	GrossAmount   int64
	NetAmount     int64
	Currency      string
	CreatedAt     time.Time
	SubmittedAt   *time.Time
	ConfirmedAt   *time.Time
}

// MerchantPayout represents the net amount to be disbursed to a merchant.
type MerchantPayout struct {
	PayoutID     uuid.UUID
	MerchantID   string
	NetAmount    int64
	Currency     string
	Captures     int64
	Refunds      int64
	Chargebacks  int64
	Fees         int64
	ReserveHeld  int64
	PayoutMethod string
	BankDetails  BankDetails
	Status       string
	ScheduledAt  time.Time
}

// BankDetails holds the merchant's bank account information for payouts.
type BankDetails struct {
	BankName      string
	AccountNumber string // Encrypted at rest
	RoutingNumber string
	IBAN          string
	SwiftCode     string
	Country       string
}

// RunDailySettlement executes the full daily settlement cycle.
// Called by a cron job at the configured cutoff hour.
func (s *Service) RunDailySettlement(ctx context.Context) error {
	settlementDate := time.Now().UTC().Truncate(24 * time.Hour)
	s.logger.Info("Starting daily settlement", "date", settlementDate.Format("2006-01-02"))

	// Step 1: Generate clearing files per network
	networks := []struct {
		name   string
		client ClearingClient
	}{
		{"visa", s.visaClient},
		{"mastercard", s.mastercardClient},
	}

	for _, network := range networks {
		if err := s.processClearingBatch(ctx, network.name, network.client, settlementDate); err != nil {
			s.logger.Error("Clearing batch failed",
				"network", network.name,
				"date", settlementDate,
				"error", err,
			)
			batchesCreated.WithLabelValues(network.name, "failed").Inc()
			// Continue with other networks; a Visa failure should not block Mastercard
			continue
		}
		batchesCreated.WithLabelValues(network.name, "success").Inc()
	}

	lastSuccessfulBatch.Set(float64(time.Now().Unix()))
	return nil
}

// processClearingBatch generates and submits a clearing file for a specific network.
func (s *Service) processClearingBatch(
	ctx context.Context,
	network string,
	client ClearingClient,
	settlementDate time.Time,
) error {
	timer := prometheus.NewTimer(batchDuration.WithLabelValues(network))
	defer timer.ObserveDuration()

	// Query uncaptured transactions for this network
	transactions, err := s.getUnclearedTransactions(ctx, network, settlementDate)
	if err != nil {
		return fmt.Errorf("querying uncleared transactions: %w", err)
	}

	if len(transactions) == 0 {
		s.logger.Info("No transactions to clear", "network", network, "date", settlementDate)
		return nil
	}

	s.logger.Info("Processing clearing batch",
		"network", network,
		"transaction_count", len(transactions),
		"date", settlementDate,
	)

	// Create batch record
	batch := &Batch{
		BatchID:        uuid.New(),
		Network:        network,
		SettlementDate: settlementDate,
		Status:         "GENERATING",
		TransactionCount: len(transactions),
		CreatedAt:      time.Now().UTC(),
	}

	if err := s.saveBatch(ctx, batch); err != nil {
		return fmt.Errorf("saving batch record: %w", err)
	}

	// Generate clearing file
	clearingFile, err := client.GenerateClearingFile(ctx, transactions)
	if err != nil {
		batch.Status = "GENERATION_FAILED"
		s.saveBatch(ctx, batch)
		return fmt.Errorf("generating clearing file: %w", err)
	}

	// Submit to network
	batch.Status = "SUBMITTING"
	s.saveBatch(ctx, batch)

	submissionResult, err := client.SubmitClearingFile(ctx, clearingFile)
	if err != nil {
		batch.Status = "SUBMISSION_FAILED"
		s.saveBatch(ctx, batch)
		return fmt.Errorf("submitting clearing file: %w", err)
	}

	now := time.Now().UTC()
	batch.Status = "SUBMITTED"
	batch.SubmittedAt = &now
	s.saveBatch(ctx, batch)

	// Mark transactions as clearing_submitted
	if err := s.markTransactionsClearing(ctx, transactions, batch.BatchID); err != nil {
		return fmt.Errorf("marking transactions as clearing: %w", err)
	}

	s.logger.Info("Clearing batch submitted",
		"network", network,
		"batch_id", batch.BatchID,
		"transaction_count", len(transactions),
		"submission_ref", submissionResult.ReferenceID,
	)

	// Publish event
	s.eventPublisher.Publish(ctx, "settlement.clearing.submitted", batch.BatchID.String(), map[string]interface{}{
		"batch_id":          batch.BatchID.String(),
		"network":           network,
		"transaction_count": len(transactions),
		"settlement_date":   settlementDate.Format("2006-01-02"),
	})

	return nil
}

// ComputeMerchantPayouts calculates net settlement amounts per merchant.
// Called after clearing confirmation is received from the card networks.
func (s *Service) ComputeMerchantPayouts(ctx context.Context, settlementDate time.Time) ([]MerchantPayout, error) {
	s.logger.Info("Computing merchant payouts", "date", settlementDate.Format("2006-01-02"))

	rows, err := s.db.QueryContext(ctx, `
		SELECT
			t.merchant_id,
			t.currency,
			COALESCE(SUM(CASE WHEN t.type = 'CAPTURE' THEN t.net_amount ELSE 0 END), 0) AS captures,
			COALESCE(SUM(CASE WHEN t.type = 'REFUND' THEN t.net_amount ELSE 0 END), 0) AS refunds,
			COALESCE(SUM(CASE WHEN t.type = 'CHARGEBACK' THEN t.amount + t.chargeback_fee ELSE 0 END), 0) AS chargebacks,
			COALESCE(SUM(t.interchange_fee + t.scheme_fee + t.processing_fee), 0) AS total_fees
		FROM cleared_transactions t
		WHERE t.settlement_date = $1
		  AND t.clearing_status = 'CONFIRMED'
		GROUP BY t.merchant_id, t.currency
	`, settlementDate)
	if err != nil {
		return nil, fmt.Errorf("querying settled transactions: %w", err)
	}
	defer rows.Close()

	var payouts []MerchantPayout

	for rows.Next() {
		var merchantID, currency string
		var captures, refunds, chargebacks, fees int64

		if err := rows.Scan(&merchantID, &currency, &captures, &refunds, &chargebacks, &fees); err != nil {
			return nil, fmt.Errorf("scanning payout row: %w", err)
		}

		netAmount := captures - refunds - chargebacks

		// Check minimum payout threshold
		minThreshold, ok := s.config.MinPayoutThreshold[currency]
		if !ok {
			minThreshold = 1000 // Default: $10.00
		}
		if netAmount < minThreshold {
			s.logger.Info("Payout below threshold, rolling to next cycle",
				"merchant", merchantID,
				"net_amount", netAmount,
				"threshold", minThreshold,
				"currency", currency,
			)
			continue
		}

		// Apply rolling reserve if configured for this merchant
		reserveHeld := s.computeReserve(ctx, merchantID, netAmount)
		payoutAmount := netAmount - reserveHeld

		// Load merchant bank details
		bankDetails, err := s.getMerchantBankDetails(ctx, merchantID)
		if err != nil {
			s.logger.Error("Failed to load bank details, skipping payout",
				"merchant", merchantID,
				"error", err,
			)
			continue
		}

		payout := MerchantPayout{
			PayoutID:     uuid.New(),
			MerchantID:   merchantID,
			NetAmount:    payoutAmount,
			Currency:     currency,
			Captures:     captures,
			Refunds:      refunds,
			Chargebacks:  chargebacks,
			Fees:         fees,
			ReserveHeld:  reserveHeld,
			PayoutMethod: s.determinePayoutMethod(bankDetails),
			BankDetails:  bankDetails,
			Status:       "SCHEDULED",
			ScheduledAt:  time.Now().UTC().Add(24 * time.Hour),
		}

		payouts = append(payouts, payout)
	}

	s.logger.Info("Merchant payouts computed",
		"date", settlementDate.Format("2006-01-02"),
		"merchant_count", len(payouts),
	)

	return payouts, nil
}

// ExecutePayouts submits payout instructions to the banking partner.
func (s *Service) ExecutePayouts(ctx context.Context, payouts []MerchantPayout) error {
	for _, payout := range payouts {
		if err := s.bankingClient.SubmitPayout(ctx, PayoutInstruction{
			PayoutID:      payout.PayoutID,
			MerchantID:    payout.MerchantID,
			Amount:        payout.NetAmount,
			Currency:      payout.Currency,
			BankDetails:   payout.BankDetails,
			PayoutMethod:  payout.PayoutMethod,
		}); err != nil {
			s.logger.Error("Payout submission failed",
				"merchant", payout.MerchantID,
				"amount", payout.NetAmount,
				"error", err,
			)
			// Mark as failed, will be retried in next cycle
			payout.Status = "FAILED"
			continue
		}

		payout.Status = "SUBMITTED"
		payoutAmount.WithLabelValues(payout.Currency).Add(float64(payout.NetAmount))

		s.logger.Info("Payout submitted",
			"merchant", payout.MerchantID,
			"amount", payout.NetAmount,
			"currency", payout.Currency,
			"method", payout.PayoutMethod,
		)
	}

	return nil
}

// computeReserve calculates the rolling reserve amount for a merchant.
// Merchants with elevated chargeback rates have a percentage of each
// settlement held back as security.
func (s *Service) computeReserve(ctx context.Context, merchantID string, netAmount int64) int64 {
	reserveConfig, err := s.getMerchantReserveConfig(ctx, merchantID)
	if err != nil || reserveConfig == nil {
		return 0
	}

	reserveAmount := int64(float64(netAmount) * reserveConfig.Percentage)
	if reserveAmount > reserveConfig.MaxReserve {
		reserveAmount = reserveConfig.MaxReserve
	}

	return reserveAmount
}

func (s *Service) determinePayoutMethod(bank BankDetails) string {
	if bank.IBAN != "" && bank.Country != "" {
		// International: use SWIFT
		return "SWIFT"
	}
	if bank.Country == "IN" {
		return "NEFT"
	}
	if bank.Country == "US" {
		return "ACH"
	}
	return "WIRE"
}

// ─── Interfaces ───

type ClearingClient interface {
	GenerateClearingFile(ctx context.Context, transactions []ClearedTransaction) (*ClearingFile, error)
	SubmitClearingFile(ctx context.Context, file *ClearingFile) (*SubmissionResult, error)
}

type PayoutClient interface {
	SubmitPayout(ctx context.Context, instruction PayoutInstruction) error
}

type EventPublisher interface {
	Publish(ctx context.Context, topic string, key string, payload map[string]interface{}) error
}

// ─── Supporting Types ───

type ClearedTransaction struct {
	TransactionID    uuid.UUID
	MerchantID       string
	Amount           int64
	Currency         string
	CardNetwork      string
	AuthorizationCode string
	CapturedAt       time.Time
}

type ClearingFile struct {
	Network   string
	Content   []byte
	Checksum  string
	Signature []byte
}

type SubmissionResult struct {
	ReferenceID string
	AcceptedAt  time.Time
}

type PayoutInstruction struct {
	PayoutID     uuid.UUID
	MerchantID   string
	Amount       int64
	Currency     string
	BankDetails  BankDetails
	PayoutMethod string
}

type ReserveConfig struct {
	Percentage  float64
	MaxReserve  int64
	HoldDays    int
}

// Stub methods for database operations (implementations depend on ORM/driver choice)
func (s *Service) getUnclearedTransactions(ctx context.Context, network string, date time.Time) ([]ClearedTransaction, error) {
	return nil, nil // Implementation uses database query
}

func (s *Service) saveBatch(ctx context.Context, batch *Batch) error {
	return nil // Implementation uses database upsert
}

func (s *Service) markTransactionsClearing(ctx context.Context, txns []ClearedTransaction, batchID uuid.UUID) error {
	return nil // Implementation uses batch update
}

func (s *Service) getMerchantBankDetails(ctx context.Context, merchantID string) (BankDetails, error) {
	return BankDetails{}, nil // Implementation uses encrypted storage
}

func (s *Service) getMerchantReserveConfig(ctx context.Context, merchantID string) (*ReserveConfig, error) {
	return nil, nil // Implementation uses merchant config table
}
