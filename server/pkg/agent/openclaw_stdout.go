package agent

import (
	"io"
	"sync"
	"time"
)

// openclawResultIdleGrace is how long stdout must stay silent *after* the
// buffer already parses as a complete openclaw result before readOpenclawStdout
// treats the run as finished.
//
// Deliberately generous. The cheap check — does the buffer parse as a complete
// result? — is the real gate; this only guards against declaring victory
// mid-write if a future openclaw flushes a result and then appends more. 2s is
// far longer than the gap inside one flush, and it costs nothing in the normal
// case because a CLI that exits reaches EOF and never consults it.
const openclawResultIdleGrace = 2 * time.Second

// openclawStdoutPoll is how often the reader re-evaluates its exit conditions.
const openclawStdoutPoll = 100 * time.Millisecond

// readOpenclawStdout drains r and returns the bytes read. It stops at EOF, or
// early once the buffer parses as a complete openclaw result AND stdout has
// been idle for idleGrace — reporting cutShort=true in that second case so the
// caller can cancel the run before waiting on the process.
//
// # Why not io.ReadAll
//
// io.ReadAll returns only at EOF, and EOF requires every write end of the pipe
// to be closed. Observed in production: `openclaw agent --local --json` printed
// its complete result blob — the agent's reply was fully generated — and then
// did not exit. The pipe stayed open, so the read never returned, the goroutine
// never reached cmd.Wait, and the finished reply sat in the daemon's buffer
// while the task held its execution slot:
//
//	T+0s     openclaw started
//	T+24s    the complete result blob was written to stdout
//	T+8min   process still alive, slot still held, user saw nothing
//
// # Why a complete result is the right boundary
//
// This exact hazard is already handled for cursor-agent, whose adapter notes
// that current versions "can emit the terminal result event but keep a worker
// process alive" and therefore treats result as the protocol boundary and
// cancels (see the "result" case in cursor.go). openclaw parses one
// whole-buffer blob rather than line events, so the equivalent condition is
// "the buffer is a complete result" instead of "a result line arrived".
//
// Both conditions are required. Idle alone is not enough — an agent may pause
// for minutes while thinking, and cutting off a partial buffer would discard
// work it has already done, which is worse than the hang. Parseable alone is
// not enough either, since more output may still be coming.
//
// Nothing is cut short before any output appears: idle time is measured from
// the last byte received, so a silent agent remains governed purely by the
// caller's context, exactly as before.
//
// On the cutShort path the internal read goroutine is still blocked in
// r.Read. It exits when the caller's cancellation closes r, which openclaw's
// Execute already arranges. Callers that do not close r on cancellation must
// not use this function.
func readOpenclawStdout(r io.Reader, idleGrace time.Duration) (buf []byte, cutShort bool, err error) {
	if idleGrace <= 0 {
		idleGrace = openclawResultIdleGrace
	}

	var (
		mu       sync.Mutex
		acc      []byte
		lastByte time.Time
		readErr  error
		atEOF    bool
	)

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		chunk := make([]byte, 32*1024)
		for {
			n, rerr := r.Read(chunk)
			// One Read is one observation: a reader is allowed to return the
			// final bytes and io.EOF together, and publishing those under two
			// separate locks would let the ticker see "bytes arrived, stream
			// still open" in between. On a loaded machine that gap can outlast
			// idleGrace, and since the trailing bytes are exactly what makes
			// the buffer parseable, the poll loop would report cutShort for a
			// stream that had already ended cleanly.
			mu.Lock()
			if n > 0 {
				acc = append(acc, chunk[:n]...)
				lastByte = time.Now()
			}
			if rerr != nil {
				if rerr != io.EOF {
					readErr = rerr
				}
				atEOF = true
			}
			mu.Unlock()
			if rerr != nil {
				return
			}
		}
	}()

	ticker := time.NewTicker(openclawStdoutPoll)
	defer ticker.Stop()

	for {
		select {
		case <-finished:
			// The stream ended on its own: the pre-existing behaviour, with the
			// same buffer io.ReadAll would have produced.
			mu.Lock()
			out, rerr := acc, readErr
			mu.Unlock()
			return out, false, rerr

		case <-ticker.C:
			// Decide and snapshot under ONE lock. Re-reading acc after
			// releasing it would mix a stale verdict with a fresh buffer: the
			// terminal write can land in between, so the "still open, gone
			// quiet" check would be answered from before it while the bytes
			// being parsed came from after it. That is precisely the buffer
			// that parses, so a cleanly-ended stream got reported as cut
			// short, spuriously warning and cancelling a process that had
			// already exited.
			//
			// The threshold is still evaluated before copying, so the common
			// tick allocates nothing: copying the whole accumulated result on
			// every tick would repeat ~20 times per wait window for no reason.
			mu.Lock()
			var out []byte
			if !atEOF && len(acc) > 0 && time.Since(lastByte) >= idleGrace {
				out = append([]byte(nil), acc...)
			}
			mu.Unlock()

			// Either the stream has ended — let the <-finished branch report
			// the final state — or it has not been quiet long enough yet.
			if out == nil {
				continue
			}
			if _, ok := parseWholeBufferOpenclawResult(out); !ok {
				continue
			}
			return out, true, nil
		}
	}
}
