package gitproxy

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // a pack's own checksum, not a security check
	"crypto/sha256"
	"hash"
	"strconv"
)

// responseCheck reads a protocol v2 fetch response as it passes, and says
// whether it is whole. GitHub answers 200 before it knows how a pack will go, so
// a failure arrives inside the stream: an ERR line, a message on sideband 3, or
// a stream that stops. A stored or served entry can also be damaged, so the
// pack's own checksum is verified. Any of these would fail a fetch.
type responseCheck struct {
	bad      bool
	sawPack  bool
	inPack   bool
	finished bool // the last packet was the closing flush

	header  [4]byte
	headerN int
	left    int    // payload bytes still to come in this packet
	start   []byte // the payload's first bytes
	started bool   // the sideband byte of this packet has passed

	sum   hash.Hash // of the pack, less its trailer
	tail  []byte    // the last bytes seen, the trailer once the pack ends
	magic []byte    // the pack's first bytes
}

func newResponseCheck(objectFormat string) *responseCheck {
	if objectFormat == "sha256" {
		return &responseCheck{sum: sha256.New()}
	}
	return &responseCheck{sum: sha1.New()} //nolint:gosec // see the import
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
			if c.inPack {
				c.packPayload(p[:take])
			} else if room := startLen - len(c.start); room > 0 {
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
		c.started = false
	}
}

func (c *responseCheck) endPacket() {
	switch {
	case c.inPack:
	case bytes.HasPrefix(c.start, []byte("ERR ")):
		c.bad = true
	case string(c.start) == "packfile\n" || string(c.start) == "packfile":
		c.sawPack, c.inPack = true, true
	}
}

// packPayload reads part of a packet in the packfile section. Its first byte is
// the sideband: 1 is the pack, 2 is progress, and anything else a failure.
func (c *responseCheck) packPayload(p []byte) {
	if !c.started {
		c.started = true
		if p[0] != 1 && p[0] != 2 {
			c.bad = true
			return
		}
		c.start = append(c.start[:0], p[0])
		p = p[1:]
	}
	if c.start[0] != 1 || len(p) == 0 {
		return
	}
	if room := 4 - len(c.magic); room > 0 {
		c.magic = append(c.magic, p[:min(room, len(p))]...)
	}
	// The last bytes are the trailer, which the checksum leaves out.
	c.tail = append(c.tail, p...)
	if over := len(c.tail) - c.sum.Size(); over > 0 {
		c.sum.Write(c.tail[:over])
		c.tail = append(c.tail[:0], c.tail[over:]...)
	}
}

// complete reports whether what passed was a whole, successful response.
func (c *responseCheck) complete() bool {
	return !c.bad && c.sawPack && c.finished && c.left == 0 && c.headerN == 0 &&
		string(c.magic) == "PACK" && bytes.Equal(c.tail, c.sum.Sum(nil))
}
