// Copyright (c) 2024 XDC Network
// Countdown timer with exponential backoff for XDPoS 2.0 timeout mechanism

package countdown

import (
	"math"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/log"
)

// CountdownTimer is an interface for countdown timers
type CountdownTimer interface {
	Reset(chain interface{}, currentRound, qcRound uint64)
	SetParams(duration time.Duration, base float64, maxExponent int) error
	Stop()
}

// ExpCountDown is an exponential countdown timer that increases timeout
// duration on each consecutive timeout until a QC is received
type ExpCountDown struct {
	lock sync.RWMutex

	// Configuration
	baseDuration time.Duration
	base         float64 // Exponential base (e.g., 2.0)
	maxExponent  int     // Maximum exponent to prevent infinite growth

	// State
	timer        *time.Timer
	currentRound uint64
	qcRound      uint64
	running      bool

	// Callback
	OnTimeoutFn func(time time.Time, chain interface{}) error
	chain       interface{}
}

// NewExpCountDown creates a new exponential countdown timer
func NewExpCountDown(duration time.Duration, base float64, maxExponent int) (*ExpCountDown, error) {
	if base < 1.0 {
		base = 1.0
	}
	if maxExponent < 0 {
		maxExponent = 0
	}

	return &ExpCountDown{
		baseDuration: duration,
		base:         base,
		maxExponent:  maxExponent,
		running:      false,
	}, nil
}

// SetParams updates the timer parameters
func (e *ExpCountDown) SetParams(duration time.Duration, base float64, maxExponent int) error {
	e.lock.Lock()
	defer e.lock.Unlock()

	e.baseDuration = duration
	if base >= 1.0 {
		e.base = base
	}
	if maxExponent >= 0 {
		e.maxExponent = maxExponent
	}
	return nil
}

// Reset restarts the countdown timer with the current round state
func (e *ExpCountDown) Reset(chain interface{}, currentRound, qcRound uint64) {
	e.lock.Lock()
	defer e.lock.Unlock()

	// Stop existing timer
	if e.timer != nil {
		e.timer.Stop()
	}

	e.chain = chain
	e.currentRound = currentRound
	e.qcRound = qcRound

	// Calculate timeout duration with exponential backoff
	// Exponent is based on how many rounds since last QC
	roundDiff := int(currentRound - qcRound)
	if roundDiff < 0 {
		roundDiff = 0
	}
	if roundDiff > e.maxExponent {
		roundDiff = e.maxExponent
	}

	multiplier := math.Pow(e.base, float64(roundDiff))
	timeout := time.Duration(float64(e.baseDuration) * multiplier)

	log.Debug("Countdown timer reset",
		"currentRound", currentRound,
		"qcRound", qcRound,
		"roundDiff", roundDiff,
		"timeout", timeout,
	)

	e.running = true
	e.timer = time.AfterFunc(timeout, func() {
		e.onTimeout()
	})
}

// Stop stops the countdown timer
func (e *ExpCountDown) Stop() {
	e.lock.Lock()
	defer e.lock.Unlock()

	if e.timer != nil {
		e.timer.Stop()
	}
	e.running = false
}

// onTimeout is called when the timer expires
func (e *ExpCountDown) onTimeout() {
	e.lock.RLock()
	chain := e.chain
	callback := e.OnTimeoutFn
	e.lock.RUnlock()

	if callback != nil && chain != nil {
		err := callback(time.Now(), chain)
		if err != nil {
			log.Error("Countdown timeout callback error", "err", err)
		}
	}

	// Restart with increased timeout
	e.lock.Lock()
	if e.running {
		e.currentRound++
		e.lock.Unlock()
		e.Reset(chain, e.currentRound, e.qcRound)
	} else {
		e.lock.Unlock()
	}
}

// IsRunning returns whether the timer is running
func (e *ExpCountDown) IsRunning() bool {
	e.lock.RLock()
	defer e.lock.RUnlock()
	return e.running
}

// GetTimeout returns the current timeout duration
func (e *ExpCountDown) GetTimeout() time.Duration {
	e.lock.RLock()
	defer e.lock.RUnlock()

	roundDiff := int(e.currentRound - e.qcRound)
	if roundDiff < 0 {
		roundDiff = 0
	}
	if roundDiff > e.maxExponent {
		roundDiff = e.maxExponent
	}

	multiplier := math.Pow(e.base, float64(roundDiff))
	return time.Duration(float64(e.baseDuration) * multiplier)
}
