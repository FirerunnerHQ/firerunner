package gitproxy

import (
	"bytes"
	"strconv"
)

// responseCheck reads a protocol v2 fetch response as it passes, and says
// whether it is whole. GitHub answers 200 before it knows how a pack will go, so
// a failure arrives inside the stream: an ERR line, a message on sideband 3, or
// a stream that stops. Stored, any of these would fail every later fetch.
type responseCheck struct {
	bad      bool
	sawPack  bool
	inPack   bool
	finished bool // the last packet was the closing flush

	header  [4]byte
	headerN int
	left    int    // payload bytes still to come in this packet
	start   []byte // the payload's first bytes
}

// startLen is enough to recognise "packfile\n" and "ERR ".
const startLen = 9

func (c *responseCheck) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 && !c.bad {
		if c.finished {
			c.bad = true // anything after the closing flush
			break
		}
		if c.left > 0 {
			take := min(c.left, len(p))
			if room := startLen - len(c.start); room > 0 {
				c.start = append(c.start, p[:min(room, take)]...)
			}
			c.left -= take
			p = p[take:]
			if c.left == 0 {
				c.endPacket()
			}
			continue
		}
		take := copy(c.header[c.headerN:], p)
		c.headerN += take
		p = p[take:]
		if c.headerN == len(c.header) {
			c.headerN = 0
			c.startPacket()
		}
	}
	return n, nil
}

func (c *responseCheck) startPacket() {
	size, err := strconv.ParseUint(string(c.header[:]), 16, 16)
	switch {
	case err != nil:
		c.bad = true
	case size == 0:
		c.finished = true
	case size == 1: // a delimiter between sections
	case size < 5:
		c.bad = true
	default:
		c.left = int(size) - 4
		c.start = c.start[:0]
	}
}

func (c *responseCheck) endPacket() {
	switch {
	case c.inPack:
		if len(c.start) > 0 && c.start[0] == 3 {
			c.bad = true
		}
	case bytes.HasPrefix(c.start, []byte("ERR ")):
		c.bad = true
	case string(c.start) == "packfile\n" || string(c.start) == "packfile":
		c.sawPack, c.inPack = true, true
	}
}

// complete reports whether what passed was a whole, successful response.
func (c *responseCheck) complete() bool {
	return !c.bad && c.sawPack && c.finished && c.left == 0 && c.headerN == 0
}
