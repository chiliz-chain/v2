package metrics

import (
    "sync"
    "strings"
)

// LabelledCounter is a labelled collection of counters.
type LabelledCounter struct {
    name   string
    Labels []string
    mu     sync.RWMutex
    counters map[string]Counter
}

// Helper to join label values into a unique key
func labelKey(labelValues []string) string {
    return strings.Join(labelValues, "|")
}

// NewLabelledCounter creates a new LabelledCounter.
func NewLabelledCounter(name string, labels []string) *LabelledCounter {
    return &LabelledCounter{
        name: name,
        Labels: labels,
        counters: make(map[string]Counter),
    }
}

// NewRegisteredLabelledCounter creates a new registered LabelledCounter.
func NewRegisteredLabelledCounter(name string, labels []string, r Registry) *LabelledCounter {
	lc := NewLabelledCounter(name, labels)
	if nil == r {
		r = DefaultRegistry
	}
	r.Register(name, lc)
	return lc
}

// With returns the Counter for the given label values
func (cv *LabelledCounter) With(labelValues ...string) Counter {
    key := labelKey(labelValues)
    cv.mu.RLock()
    c, ok := cv.counters[key]
    cv.mu.RUnlock()
    if ok {
        return c
    }
    cv.mu.Lock()
    defer cv.mu.Unlock()
    if c, ok := cv.counters[key]; ok {
        return c
    }
    c = NewCounter()
    cv.counters[key] = c
    return c
}

// Each iterates over all counters and their label values
func (cv *LabelledCounter) Each(f func(labelValues []string, c Counter)) {
    cv.mu.RLock()
    defer cv.mu.RUnlock()
    for key, c := range cv.counters {
        labelValues := strings.Split(key, "|")
        f(labelValues, c)
    }
}
