package imapbackend

import (
	"net"
	"sync/atomic"
)

// Stats accumulates wire bytes transferred over an account's IMAP and SMTP
// connections, for the status bar's "data transferred" metric.
type Stats struct {
	bytesIn  atomic.Int64
	bytesOut atomic.Int64
}

// Transferred returns cumulative bytes received and sent.
func (s *Stats) Transferred() (in, out int64) {
	return s.bytesIn.Load(), s.bytesOut.Load()
}

// Transferred exposes the backend's byte counters (implements the stats interface
// the launcher's status bar aggregates).
func (b *Backend) Transferred() (in, out int64) { return b.stats.Transferred() }

// countingConn wraps a net.Conn to count bytes read and written. Wrapping the raw
// TCP conn (below TLS) counts the actual wire bytes.
type countingConn struct {
	net.Conn
	stats *Stats
}

func (c *countingConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.stats.bytesIn.Add(int64(n))
	return n, err
}

func (c *countingConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.stats.bytesOut.Add(int64(n))
	return n, err
}
