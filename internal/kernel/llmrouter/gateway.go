package llmrouter

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/jinyitao123/weave/internal/base/execution"
)

var ErrGatewayQueueFull = errors.New("model gateway queue is full")
var ErrGatewayTokenBudget = errors.New("model request exceeds gateway token burst")

type GatewayPolicy struct {
	ProviderID       string  `json:"provider_id"`
	Model            string  `json:"model"`
	MaxInFlight      int     `json:"max_in_flight"`
	RPM              int     `json:"rpm,omitempty"`
	TPM              int     `json:"tpm,omitempty"`
	Share            float64 `json:"share,omitempty"`
	MinInFlight      int     `json:"min_in_flight,omitempty"`
	Retry429         *bool   `json:"retry_429,omitempty"`
	Adaptive         *bool   `json:"adaptive,omitempty"`
	DecreaseFactor   float64 `json:"decrease_factor,omitempty"`
	RecoverySeconds  int     `json:"recovery_seconds,omitempty"`
	TimeoutThreshold float64 `json:"timeout_threshold,omitempty"`
}
type GatewayConfig struct {
	Limits []GatewayPolicy `json:"limits"`
}
type gatewayKey struct {
	provider, model, endpoint string
	credential                [32]byte
}
type ModelGateway struct {
	mu         sync.Mutex
	policies   []GatewayPolicy
	buckets    map[gatewayKey]*modelBucket
	transports map[string]*http.Transport
	closed     bool
}

func LoadModelGateway(path string) (*ModelGateway, error) {
	if path == "" {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	var config GatewayConfig
	if err := decoder.Decode(&config); err != nil {
		return nil, err
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, errors.New("gateway configuration contains trailing data")
	}
	return NewModelGateway(config)
}

func NewModelGateway(config GatewayConfig) (*ModelGateway, error) {
	if len(config.Limits) > 128 {
		return nil, errors.New("too many gateway policies")
	}
	seen := map[string]bool{}
	for i := range config.Limits {
		p := &config.Limits[i]
		if p.Model == "" {
			p.Model = "*"
		}
		if p.Share == 0 {
			p.Share = 1
		}
		if p.MinInFlight == 0 {
			p.MinInFlight = 1
		}
		if p.DecreaseFactor == 0 {
			p.DecreaseFactor = .7
		}
		if p.RecoverySeconds == 0 {
			p.RecoverySeconds = 10
		}
		if p.TimeoutThreshold == 0 {
			p.TimeoutThreshold = .2
		}
		key := p.ProviderID + "\x00" + p.Model
		if p.ProviderID == "" || seen[key] || p.MaxInFlight < 1 || p.MaxInFlight > 20000 || p.MinInFlight < 1 || p.MinInFlight > p.MaxInFlight || p.RPM < 0 || p.TPM < 0 || math.IsNaN(p.Share) || p.Share <= 0 || p.Share > 1 {
			return nil, errors.New("invalid or duplicate gateway policy")
		}
		if !finite(p.DecreaseFactor) || p.DecreaseFactor <= 0 || p.DecreaseFactor >= 1 || p.RecoverySeconds < 1 || p.RecoverySeconds > 3600 || !finite(p.TimeoutThreshold) || p.TimeoutThreshold <= 0 || p.TimeoutThreshold > 1 || float64(p.MaxInFlight)*p.Share < 1 || (p.RPM > 0 && float64(p.RPM)*p.Share < 1) || (p.TPM > 0 && float64(p.TPM)*p.Share < 1) {
			return nil, errors.New("gateway share or adaptive policy is out of range")
		}
		seen[key] = true
	}
	return &ModelGateway{policies: append([]GatewayPolicy(nil), config.Limits...), buckets: make(map[gatewayKey]*modelBucket), transports: make(map[string]*http.Transport)}, nil
}

func (g *ModelGateway) Close() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	g.closed = true
	for _, bucket := range g.buckets {
		close(bucket.stop)
	}
	for _, transport := range g.transports {
		transport.CloseIdleConnections()
	}
}

func (g *ModelGateway) transport(endpoint string) http.RoundTripper {
	g.mu.Lock()
	defer g.mu.Unlock()
	if transport := g.transports[endpoint]; transport != nil {
		return transport
	}
	capacity := 0
	for _, p := range g.policies {
		capacity += max(1, int(float64(p.MaxInFlight)*p.Share))
	}
	capacity = max(16, capacity)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxConnsPerHost = capacity
	transport.MaxIdleConnsPerHost = capacity
	transport.MaxIdleConns = capacity * 2
	g.transports[endpoint] = transport
	return transport
}

func (g *ModelGateway) bucket(config ProviderConfig, model string) *modelBucket {
	if g == nil {
		return nil
	}
	provider := config.GatewayProviderID
	if provider == "" {
		provider = config.ID
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var policy *GatewayPolicy
	for i := range g.policies {
		p := &g.policies[i]
		if p.ProviderID == provider && (p.Model == model || p.Model == "*") {
			policy = p
			if p.Model == model {
				break
			}
		}
	}
	if policy == nil {
		return nil
	}
	key := gatewayKey{provider: provider, model: model, endpoint: config.BaseURL, credential: sha256.Sum256([]byte(config.APIKey))}
	if bucket := g.buckets[key]; bucket != nil {
		return bucket
	}
	p := *policy
	p.MaxInFlight = max(1, int(float64(p.MaxInFlight)*p.Share))
	p.MinInFlight = min(p.MinInFlight, p.MaxInFlight)
	b := &modelBucket{policy: p, limit: p.MaxInFlight, lastRefill: time.Now(), lastCongestion: time.Now(), lastWindow: time.Now(), wake: make(chan struct{}, 1), stop: make(chan struct{}), deficits: make(map[string]int)}
	b.rpmCapacity = float64(p.RPM) * p.Share
	b.tpmCapacity = float64(p.TPM) * p.Share
	b.rpmTokens = b.rpmCapacity
	b.tpmTokens = b.tpmCapacity
	g.buckets[key] = b
	if g.closed {
		close(b.stop)
	} else {
		go b.run()
	}
	return b
}

type modelWaiter struct {
	ctx      context.Context
	priority int
	fair     string
	tokens   int
	ready    chan struct{}
	granted  bool
}
type modelBucket struct {
	mu                                             sync.Mutex
	policy                                         GatewayPolicy
	limit, inFlight                                int
	waiters                                        []*modelWaiter
	lastFair                                       string
	deficits                                       map[string]int
	rpmCapacity, tpmCapacity, rpmTokens, tpmTokens float64
	lastRefill, lastCongestion, blockedUntil       time.Time
	lastWindow                                     time.Time
	completed, timeouts                            int
	wake, stop                                     chan struct{}
}

func (b *modelBucket) signal() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}
func (b *modelBucket) acquire(ctx context.Context, tokens int) (func(*int, error), error) {
	hints := execution.SchedulingFromContext(ctx)
	fair := hints.FairnessKey
	if fair == "" {
		if subject, ok := execution.SubjectFromContext(ctx); ok {
			fair = subject.WorkspaceID
		}
	}
	if fair == "" {
		fair = "normal"
	}
	if subject, ok := execution.SubjectFromContext(ctx); ok {
		fair = subject.WorkspaceID + "/" + fair
	}
	w := &modelWaiter{ctx: ctx, priority: hints.Priority, fair: fair, tokens: tokens, ready: make(chan struct{})}
	b.mu.Lock()
	if len(b.waiters) >= 65536 {
		b.mu.Unlock()
		return nil, ErrGatewayQueueFull
	}
	if b.policy.TPM > 0 && float64(tokens) > b.tpmCapacity {
		b.mu.Unlock()
		return nil, ErrGatewayTokenBudget
	}
	b.waiters = append(b.waiters, w)
	b.mu.Unlock()
	b.signal()
	select {
	case <-w.ready:
		var once sync.Once
		return func(actual *int, err error) { once.Do(func() { b.release(tokens, actual, err) }) }, nil
	case <-ctx.Done():
		b.mu.Lock()
		granted := w.granted
		b.mu.Unlock()
		if granted {
			zero := 0
			b.release(tokens, &zero, nil)
		}
		b.signal()
		return nil, ctx.Err()
	case <-b.stop:
		return nil, context.Canceled
	}
}

func (b *modelBucket) run() {
	for {
		b.mu.Lock()
		now := time.Now()
		elapsed := now.Sub(b.lastRefill).Seconds()
		b.lastRefill = now
		b.rpmTokens = math.Min(b.rpmCapacity, b.rpmTokens+elapsed*b.rpmCapacity/60)
		b.tpmTokens = math.Min(b.tpmCapacity, b.tpmTokens+elapsed*b.tpmCapacity/60)
		kept := b.waiters[:0]
		for _, w := range b.waiters {
			if w.ctx.Err() == nil {
				kept = append(kept, w)
			}
		}
		b.waiters = kept
		accounts := make(map[string]bool, len(b.waiters))
		for _, w := range b.waiters {
			accounts[strconv.Itoa(w.priority)+"/"+w.fair] = true
		}
		for account := range b.deficits {
			if !accounts[account] {
				delete(b.deficits, account)
			}
		}
		if enabled(b.policy.Adaptive) && len(b.waiters) > 0 && b.limit < b.policy.MaxInFlight && now.Sub(b.lastCongestion) >= time.Duration(b.policy.RecoverySeconds)*time.Second {
			b.limit++
			b.lastCongestion = now
		}
		for now.After(b.blockedUntil) && b.inFlight < b.limit && len(b.waiters) > 0 && (b.policy.RPM == 0 || b.rpmTokens >= 1) {
			index := b.nextWaiter()
			if index < 0 {
				break
			}
			w := b.waiters[index]
			b.waiters = append(b.waiters[:index], b.waiters[index+1:]...)
			b.inFlight++
			w.granted = true
			if b.policy.RPM > 0 {
				b.rpmTokens--
			}
			if b.policy.TPM > 0 {
				b.tpmTokens -= float64(w.tokens)
			}
			close(w.ready)
		}
		waiting := len(b.waiters) > 0
		delay := time.Second
		if b.blockedUntil.After(now) {
			delay = b.blockedUntil.Sub(now)
		} else if waiting && b.inFlight < b.limit {
			delay = 100 * time.Millisecond
		}
		b.mu.Unlock()
		var timer *time.Timer
		var tick <-chan time.Time
		if waiting {
			timer = time.NewTimer(delay)
			tick = timer.C
		}
		select {
		case <-b.wake:
		case <-tick:
		case <-b.stop:
			if timer != nil {
				timer.Stop()
			}
			return
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// Deficit round-robin within the highest waiting priority. Each fairness key
// receives the same token quantum; in-flight calls are never preempted.
func (b *modelBucket) nextWaiter() int {
	priority := b.waiters[0].priority
	for _, w := range b.waiters {
		priority = max(priority, w.priority)
	}
	keys := []string{}
	first := map[string]int{}
	for i, w := range b.waiters {
		if w.priority != priority {
			continue
		}
		if _, ok := first[w.fair]; !ok {
			keys = append(keys, w.fair)
			first[w.fair] = i
		}
	}
	start := 0
	for i, key := range keys {
		if key == b.lastFair {
			start = (i + 1) % len(keys)
		}
	}

	maxInt := int(^uint(0) >> 1)
	steps := maxInt
	for _, key := range keys {
		w := b.waiters[first[key]]
		if b.policy.TPM > 0 && b.tpmTokens < float64(w.tokens) {
			continue
		}
		missing := max(0, w.tokens-b.deficits[strconv.Itoa(priority)+"/"+key])
		needed := missing / 4096
		if missing%4096 > 0 {
			needed++
		}
		steps = min(steps, needed)
	}
	if steps == maxInt {
		return -1
	}
	credit := maxInt
	if steps <= maxInt/4096 {
		credit = steps * 4096
	}
	for _, key := range keys {
		account := strconv.Itoa(priority) + "/" + key
		b.deficits[account] += min(credit, maxInt-b.deficits[account])
	}
	for offset := 0; offset < len(keys); offset++ {
		key := keys[(start+offset)%len(keys)]
		index := first[key]
		w := b.waiters[index]
		account := strconv.Itoa(priority) + "/" + key
		if b.deficits[account] >= w.tokens && (b.policy.TPM == 0 || b.tpmTokens >= float64(w.tokens)) {
			b.deficits[account] -= w.tokens
			b.lastFair = key
			return index
		}
	}

	return -1
}

func (b *modelBucket) release(reserved int, actual *int, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.inFlight--
	if actual != nil && b.policy.TPM > 0 {
		b.tpmTokens = math.Min(b.tpmCapacity, b.tpmTokens+float64(reserved-*actual))
	}
	status, retry := gatewayHTTPFailure(err)
	if !errors.Is(err, context.Canceled) {
		b.completed++
		var network net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &network) && network.Timeout()) {
			b.timeouts++
		}
	}
	now := time.Now()
	if now.Sub(b.lastWindow) >= time.Duration(b.policy.RecoverySeconds)*time.Second && b.completed >= 5 {
		if enabled(b.policy.Adaptive) && float64(b.timeouts)/float64(b.completed) > b.policy.TimeoutThreshold {
			b.limit = max(b.policy.MinInFlight, int(float64(b.limit)*b.policy.DecreaseFactor))
			b.lastCongestion = now
		}
		b.completed, b.timeouts = 0, 0
		b.lastWindow = now
	}
	if status == 429 || status == 503 || status == 529 {
		if enabled(b.policy.Adaptive) {
			b.limit = max(b.policy.MinInFlight, int(float64(b.limit)*b.policy.DecreaseFactor))
			b.lastCongestion = time.Now()
		}
		pause := 500 * time.Millisecond
		if retry != "" {
			pause = parseRetryAfter(retry)
		}
		b.blockedUntil = time.Now().Add(pause)
	}
	b.signal()
}

type httpRejection interface {
	HTTPStatus() int
	RetryAfterValue() string
}

func gatewayHTTPFailure(err error) (int, string) {
	var rejected httpRejection
	if errors.As(err, &rejected) {
		return rejected.HTTPStatus(), rejected.RetryAfterValue()
	}
	return 0, ""
}
func enabled(value *bool) bool { return value == nil || *value }

// gatewayMaxRetryAfter bounds how long one provider reply can pause a whole
// bucket; a caller that cannot wait that long still meets its own deadline.
const gatewayMaxRetryAfter = 60 * time.Second

func parseRetryAfter(value string) time.Duration {
	if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds >= 0 && !math.IsInf(seconds, 0) && !math.IsNaN(seconds) {
		return time.Duration(math.Min(seconds, gatewayMaxRetryAfter.Seconds()) * float64(time.Second))
	}
	if date, err := http.ParseTime(value); err == nil {
		return min(max(0, time.Until(date)), gatewayMaxRetryAfter)
	}
	return 500 * time.Millisecond
}

type gatewayContextKey struct{}

func WithModelGateway(ctx context.Context, g *ModelGateway) context.Context {
	return context.WithValue(ctx, gatewayContextKey{}, g)
}
func ModelGatewayFromContext(ctx context.Context) *ModelGateway {
	g, _ := ctx.Value(gatewayContextKey{}).(*ModelGateway)
	return g
}

func (g *ModelGateway) String() string {
	if g == nil {
		return "model gateway disabled"
	}
	return fmt.Sprintf("model gateway: %d policies", len(g.policies))
}
