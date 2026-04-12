package idgen

import (
	"fmt"
	"sync"
	"time"
)

const (
	workerIDBits     = 5  // 5 bits for worker ID
	datacenterIDBits = 5  // 5 bits for datacenter ID
	sequenceBits     = 12 // 12 bits for sequence

	maxWorkerID     = -1 ^ (-1 << workerIDBits)     // 31
	maxDatacenterID = -1 ^ (-1 << datacenterIDBits) // 31
	sequenceMask    = -1 ^ (-1 << sequenceBits)     // 4095

	workerIDShift      = sequenceBits
	datacenterIDShift  = sequenceBits + workerIDBits
	timestampLeftShift = sequenceBits + workerIDBits + datacenterIDBits

	// Twitter Snowflake epoch (Nov 04 2010 01:42:54 UTC)
	// Using the same epoch as Java Hutool for compatibility
	epoch = int64(1288834974657)
)

// Generator is a Snowflake ID generator
type Generator struct {
	mu            sync.Mutex
	lastTimestamp int64
	workerID      int64
	datacenterID  int64
	sequence      int64
}

var (
	defaultGenerator *Generator
	once             sync.Once
)

// Init initializes the default Snowflake generator.
// Should be called once at application startup.
func Init(workerID, datacenterID int64) error {
	var err error
	once.Do(func() {
		defaultGenerator, err = NewGenerator(workerID, datacenterID)
	})
	return err
}

// NewGenerator creates a new Snowflake ID generator.
func NewGenerator(workerID, datacenterID int64) (*Generator, error) {
	if workerID < 0 || workerID > maxWorkerID {
		return nil, fmt.Errorf("worker ID must be between 0 and %d", maxWorkerID)
	}
	if datacenterID < 0 || datacenterID > maxDatacenterID {
		return nil, fmt.Errorf("datacenter ID must be between 0 and %d", maxDatacenterID)
	}
	return &Generator{
		workerID:      workerID,
		datacenterID:  datacenterID,
		lastTimestamp: -1,
		sequence:      0,
	}, nil
}

// NextID generates the next Snowflake ID as int64.
func (g *Generator) NextID() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now().UnixNano()
	if now < g.lastTimestamp {
		// Clock moved backwards, wait until clock catches up
		now = g.waitNextMillis(g.lastTimestamp)
	}
	if now == g.lastTimestamp {
		g.sequence = (g.sequence + 1) & sequenceMask
		if g.sequence == 0 {
			now = g.waitNextMillis(now)
		}
	} else {
		g.sequence = 0
	}
	g.lastTimestamp = now

	return (now-epoch)<<timestampLeftShift |
		(g.datacenterID << datacenterIDShift) |
		(g.workerID << workerIDShift) |
		g.sequence
}

// NextIDStr generates the next Snowflake ID as string.
func (g *Generator) NextIDStr() string {
	return fmt.Sprintf("%d", g.NextID())
}

func (g *Generator) waitNextMillis(lastTimestamp int64) int64 {
	now := time.Now().UnixNano()
	for now <= lastTimestamp {
		now = time.Now().UnixNano()
	}
	return now
}

// Default returns the default generator.
// Panics if not initialized.
func Default() *Generator {
	if defaultGenerator == nil {
		panic("idgen: default generator not initialized, call idgen.Init() first")
	}
	return defaultGenerator
}

// NextID generates the next Snowflake ID using the default generator.
func NextID() int64 {
	return Default().NextID()
}

// NextIDStr generates the next Snowflake ID as string using the default generator.
func NextIDStr() string {
	return Default().NextIDStr()
}
