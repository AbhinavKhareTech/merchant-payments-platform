package velocity

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
)

// Service provides real-time velocity feature computation backed by Redis sorted sets.
//
// Why Go for this service:
// - Redis interactions are I/O-bound but latency-critical (sub-2ms target)
// - Goroutines provide efficient concurrency for parallel feature lookups
// - Small memory footprint per instance allows dense pod packing
// - No GC pause concerns at this latency target (unlike JVM without tuning)
//
// Data model in Redis:
// - Sorted sets keyed by card_token, scored by unix timestamp
// - Members are transaction records: "txn_id:amount:merchant_id:status"
// - TTL on keys ensures automatic cleanup (7 days)
//
// Why sorted sets over HyperLogLog or simple counters:
// - Need to compute time-windowed aggregates (count in last 1h, 6h, 24h)
// - Need to compute amount sums, not just counts
// - Need to count unique merchants (distinct member parsing)
// - Sorted set ZRANGEBYSCORE gives us all of these from a single data structure

var (
	lookupLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "velocity_lookup_duration_seconds",
			Help:    "Velocity feature lookup latency",
			Buckets: []float64{0.0005, 0.001, 0.002, 0.003, 0.005, 0.010},
		},
		[]string{"feature_group"},
	)
	lookupErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "velocity_lookup_errors_total",
			Help: "Velocity feature lookup errors",
		},
		[]string{"feature_group"},
	)
)

func init() {
	prometheus.MustRegister(lookupLatency, lookupErrors)
}

type Service struct {
	redis *redis.ClusterClient
}

func NewService(redis *redis.ClusterClient) *Service {
	return &Service{redis: redis}
}

// Features represents the velocity feature vector for a card token.
type Features struct {
	TxCount1h          int     `json:"vel_tx_count_1h"`
	TxCount6h          int     `json:"vel_tx_count_6h"`
	TxCount24h         int     `json:"vel_tx_count_24h"`
	AmountSum1h        int64   `json:"vel_amount_sum_1h"`
	AmountSum24h       int64   `json:"vel_amount_sum_24h"`
	UniqueMerchants24h int     `json:"vel_unique_merchants_24h"`
	DeclineRate7d      float64 `json:"vel_decline_rate_7d"`
}

// GetFeatures retrieves velocity features for a card token.
// All lookups run in parallel via goroutines.
// Total latency target: 2ms at p99.
func (s *Service) GetFeatures(ctx context.Context, cardToken string) (*Features, error) {
	timer := prometheus.NewTimer(lookupLatency.WithLabelValues("all"))
	defer timer.ObserveDuration()

	now := time.Now()
	key := fmt.Sprintf("vel:%s", cardToken)

	type result struct {
		field string
		value interface{}
		err   error
	}

	ch := make(chan result, 7)

	// Parallel lookups using goroutines
	go func() {
		count, err := s.countInWindow(ctx, key, now, 1*time.Hour)
		ch <- result{"tx_count_1h", count, err}
	}()
	go func() {
		count, err := s.countInWindow(ctx, key, now, 6*time.Hour)
		ch <- result{"tx_count_6h", count, err}
	}()
	go func() {
		count, err := s.countInWindow(ctx, key, now, 24*time.Hour)
		ch <- result{"tx_count_24h", count, err}
	}()
	go func() {
		sum, err := s.amountSumInWindow(ctx, key, now, 1*time.Hour)
		ch <- result{"amount_sum_1h", sum, err}
	}()
	go func() {
		sum, err := s.amountSumInWindow(ctx, key, now, 24*time.Hour)
		ch <- result{"amount_sum_24h", sum, err}
	}()
	go func() {
		count, err := s.uniqueMerchantsInWindow(ctx, key, now, 24*time.Hour)
		ch <- result{"unique_merchants_24h", count, err}
	}()
	go func() {
		rate, err := s.declineRateInWindow(ctx, key, now, 7*24*time.Hour)
		ch <- result{"decline_rate_7d", rate, err}
	}()

	features := &Features{}
	var firstErr error

	for i := 0; i < 7; i++ {
		r := <-ch
		if r.err != nil {
			lookupErrors.WithLabelValues(r.field).Inc()
			if firstErr == nil {
				firstErr = fmt.Errorf("velocity lookup %s failed: %w", r.field, r.err)
			}
			continue
		}
		switch r.field {
		case "tx_count_1h":
			features.TxCount1h = r.value.(int)
		case "tx_count_6h":
			features.TxCount6h = r.value.(int)
		case "tx_count_24h":
			features.TxCount24h = r.value.(int)
		case "amount_sum_1h":
			features.AmountSum1h = r.value.(int64)
		case "amount_sum_24h":
			features.AmountSum24h = r.value.(int64)
		case "unique_merchants_24h":
			features.UniqueMerchants24h = r.value.(int)
		case "decline_rate_7d":
			features.DeclineRate7d = r.value.(float64)
		}
	}

	// Return partial features even on errors. The ML model handles missing values.
	// Only return error if ALL lookups failed.
	if firstErr != nil && features.TxCount1h == 0 && features.TxCount24h == 0 {
		return nil, firstErr
	}

	return features, nil
}

// RecordTransaction adds a transaction to the velocity sorted set.
// Called asynchronously after authorization (not on the critical path).
func (s *Service) RecordTransaction(ctx context.Context, cardToken string, txn TransactionRecord) error {
	key := fmt.Sprintf("vel:%s", cardToken)
	member := fmt.Sprintf("%s:%d:%s:%s", txn.TransactionID, txn.AmountMinor, txn.MerchantID, txn.Status)
	score := float64(txn.Timestamp.UnixMilli())

	pipe := s.redis.Pipeline()
	pipe.ZAdd(ctx, key, redis.Z{Score: score, Member: member})
	pipe.Expire(ctx, key, 7*24*time.Hour) // 7-day TTL
	_, err := pipe.Exec(ctx)
	return err
}

func (s *Service) countInWindow(ctx context.Context, key string, now time.Time, window time.Duration) (int, error) {
	min := strconv.FormatInt(now.Add(-window).UnixMilli(), 10)
	max := strconv.FormatInt(now.UnixMilli(), 10)
	count, err := s.redis.ZCount(ctx, key, min, max).Result()
	return int(count), err
}

func (s *Service) amountSumInWindow(ctx context.Context, key string, now time.Time, window time.Duration) (int64, error) {
	min := strconv.FormatInt(now.Add(-window).UnixMilli(), 10)
	max := strconv.FormatInt(now.UnixMilli(), 10)

	members, err := s.redis.ZRangeByScore(ctx, key, &redis.ZRangeBy{
		Min: min, Max: max,
	}).Result()
	if err != nil {
		return 0, err
	}

	var sum int64
	for _, member := range members {
		amount := parseMemberAmount(member)
		sum += amount
	}
	return sum, nil
}

func (s *Service) uniqueMerchantsInWindow(ctx context.Context, key string, now time.Time, window time.Duration) (int, error) {
	min := strconv.FormatInt(now.Add(-window).UnixMilli(), 10)
	max := strconv.FormatInt(now.UnixMilli(), 10)

	members, err := s.redis.ZRangeByScore(ctx, key, &redis.ZRangeBy{
		Min: min, Max: max,
	}).Result()
	if err != nil {
		return 0, err
	}

	merchants := make(map[string]struct{})
	for _, member := range members {
		merchantID := parseMemberMerchant(member)
		merchants[merchantID] = struct{}{}
	}
	return len(merchants), nil
}

func (s *Service) declineRateInWindow(ctx context.Context, key string, now time.Time, window time.Duration) (float64, error) {
	min := strconv.FormatInt(now.Add(-window).UnixMilli(), 10)
	max := strconv.FormatInt(now.UnixMilli(), 10)

	members, err := s.redis.ZRangeByScore(ctx, key, &redis.ZRangeBy{
		Min: min, Max: max,
	}).Result()
	if err != nil {
		return 0, err
	}

	if len(members) == 0 {
		return 0, nil
	}

	var declines int
	for _, member := range members {
		status := parseMemberStatus(member)
		if status == "declined" {
			declines++
		}
	}

	return math.Round(float64(declines)/float64(len(members))*10000) / 10000, nil
}

// parseMemberAmount extracts amount from "txn_id:amount:merchant_id:status"
func parseMemberAmount(member string) int64 {
	parts := splitMember(member)
	if len(parts) < 2 {
		return 0
	}
	amount, _ := strconv.ParseInt(parts[1], 10, 64)
	return amount
}

func parseMemberMerchant(member string) string {
	parts := splitMember(member)
	if len(parts) < 3 {
		return ""
	}
	return parts[2]
}

func parseMemberStatus(member string) string {
	parts := splitMember(member)
	if len(parts) < 4 {
		return ""
	}
	return parts[3]
}

func splitMember(member string) []string {
	result := make([]string, 0, 4)
	start := 0
	for i := 0; i < len(member); i++ {
		if member[i] == ':' {
			result = append(result, member[start:i])
			start = i + 1
		}
	}
	result = append(result, member[start:])
	return result
}

type TransactionRecord struct {
	TransactionID string
	AmountMinor   int64
	MerchantID    string
	Status        string
	Timestamp     time.Time
}
