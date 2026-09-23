package shed

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAdmitsRequestsThatWaitedLittle(t *testing.T) {
	s, err := New(Config{MaxQueueLatency: 100 * time.Millisecond})
	require.NoError(t, err)

	done, ok := s.Admit(10 * time.Millisecond)
	require.True(t, ok)
	done()

	_, admitted, shedded := s.Stats()
	require.Equal(t, uint64(1), admitted)
	require.Zero(t, shedded)
}

func TestShedsRequestsThatQueuedTooLong(t *testing.T) {
	// Queue latency is the signal, not CPU: time spent waiting is a direct
	// measure of whether the service is keeping up, and it stays correct
	// when the bottleneck is a lock or a pool rather than the processor.
	s, err := New(Config{MaxQueueLatency: 100 * time.Millisecond})
	require.NoError(t, err)

	done, ok := s.Admit(150 * time.Millisecond)
	require.False(t, ok)
	require.Nil(t, done)

	_, _, shedded := s.Stats()
	require.Equal(t, uint64(1), shedded)
}

func TestConcurrencyCapIsNotRacy(t *testing.T) {
	// Checking then incrementing lets two goroutines both observe capacity
	// and both take it, which is exactly what a concurrency cap exists to
	// prevent.
	s, err := New(Config{MaxQueueLatency: time.Hour, MaxInFlight: 10})
	require.NoError(t, err)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var peak, current int

	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			done, ok := s.Admit(0)
			if !ok {
				return
			}
			mu.Lock()
			current++
			if current > peak {
				peak = current
			}
			mu.Unlock()

			time.Sleep(time.Millisecond)

			mu.Lock()
			current--
			mu.Unlock()
			done()
		}()
	}
	wg.Wait()

	require.LessOrEqual(t, peak, 10, "the in-flight cap was exceeded")
	inFlight, _, _ := s.Stats()
	require.Zero(t, inFlight, "the in-flight counter leaked")
}

func TestReleasedSlotsAreReusable(t *testing.T) {
	s, err := New(Config{MaxQueueLatency: time.Hour, MaxInFlight: 1})
	require.NoError(t, err)

	done, ok := s.Admit(0)
	require.True(t, ok)

	_, ok = s.Admit(0)
	require.False(t, ok, "the cap admitted a second concurrent request")

	done()
	done2, ok := s.Admit(0)
	require.True(t, ok, "the slot was not released")
	done2()
}

func TestInvalidConfigIsRejected(t *testing.T) {
	_, err := New(Config{MaxQueueLatency: -time.Second})
	require.Error(t, err)
	_, err = New(Config{MaxInFlight: -1})
	require.Error(t, err)
}
