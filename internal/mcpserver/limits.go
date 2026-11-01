// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"fmt"
	"sync"
)

// DefaultMaxRunning is how many investigations one caller may have running through one server
// instance (010 FR-053, research R5).
const DefaultMaxRunning = 3

// RunningLimit counts the investigations each subject has running through this instance.
type RunningLimit struct {
	max     int
	mu      sync.Mutex
	running map[string]int
}

// NewRunningLimit bounds each subject at max running investigations; max < 1 is DefaultMaxRunning.
func NewRunningLimit(max int) *RunningLimit {
	if max < 1 {
		max = DefaultMaxRunning
	}
	return &RunningLimit{max: max, running: map[string]int{}}
}

// acquire reserves one slot for subject, or says why not. The returned release frees it once.
func (l *RunningLimit) acquire(subject string) (release func(), err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running[subject] >= l.max {
		return nil, fmt.Errorf("you already have %d investigation(s) running through this server, the "+
			"bound is %d; wait for one to finish (investigation_get) before starting another",
			l.running[subject], l.max)
	}
	l.running[subject]++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.running[subject]--
			if l.running[subject] <= 0 {
				delete(l.running, subject)
			}
		})
	}, nil
}

// Running reports how many investigations subject has running.
func (l *RunningLimit) Running(subject string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.running[subject]
}
