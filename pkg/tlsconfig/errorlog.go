package tlsconfig

import (
	"bytes"
	"io"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// noisyHandshakeFragments classifies log lines that arise from clients
// negotiating with a self-signed certificate. Real TLS misconfiguration
// errors do not contain this fragment and pass through unchanged.
var noisyHandshakeFragments = []string{
	"http: TLS handshake error",
}

// silentlyDroppedFragments are sub-strings of TLS handshake errors that we
// drop entirely because they are expected and high-volume on a typical
// local-network deployment with a self-signed cert.
var silentlyDroppedFragments = []string{
	"remote error: tls: unknown certificate",
	"remote error: tls: bad certificate",
	"remote error: tls: unknown ca",
	"tls: client offered only unsupported versions",
	"tls: first record does not look like a TLS handshake",
	"EOF",
	"i/o timeout",
	"connection reset by peer",
}

// NewServerErrorLog returns a *log.Logger suitable for http.Server.ErrorLog
// that filters out routine TLS handshake noise from clients rejecting a
// self-signed certificate. Genuine TLS configuration errors pass through.
//
// The returned logger periodically emits a summary line of suppressed
// counts so operators investigating issues still know noise occurred.
func NewServerErrorLog() *log.Logger {
	w := newFilteredWriter(log.Writer())
	return log.New(w, "", log.LstdFlags|log.Lshortfile)
}

type filteredWriter struct {
	out         io.Writer
	suppressed  atomic.Uint64
	mu          sync.Mutex
	lastSummary time.Time
}

const summaryInterval = 5 * time.Minute

func newFilteredWriter(out io.Writer) *filteredWriter {
	return &filteredWriter{out: out, lastSummary: time.Now()}
}

func (w *filteredWriter) Write(p []byte) (int, error) {
	line := string(bytes.TrimRight(p, "\n"))
	if isNoisyTLSHandshake(line) {
		w.suppressed.Add(1)
		w.maybeFlushSummary()
		return len(p), nil
	}
	return w.out.Write(p)
}

func (w *filteredWriter) maybeFlushSummary() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if time.Since(w.lastSummary) < summaryInterval {
		return
	}
	count := w.suppressed.Swap(0)
	w.lastSummary = time.Now()
	if count == 0 {
		return
	}
	_, _ = w.out.Write([]byte(formatSuppressionSummary(count)))
}

func formatSuppressionSummary(count uint64) string {
	return time.Now().Format("2006/01/02 15:04:05") +
		" tlsconfig: suppressed " +
		uintToStr(count) +
		" routine TLS handshake error(s) (e.g. clients rejecting self-signed cert)\n"
}

func uintToStr(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func isNoisyTLSHandshake(line string) bool {
	matched := false
	for _, frag := range noisyHandshakeFragments {
		if strings.Contains(line, frag) {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	for _, frag := range silentlyDroppedFragments {
		if strings.Contains(line, frag) {
			return true
		}
	}
	return false
}
