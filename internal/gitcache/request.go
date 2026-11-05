// Package gitcache decides which git fetches can be served from a cache, and
// under what key.
//
// A fetch over protocol v2 is a stateless POST to git-upload-pack. When the
// client has nothing yet and names exact commits, the response depends on the
// request alone, so a stored response can answer the same request later. git
// checks every object against the commits it asked for, so a tampered entry
// fails the fetch and cannot change what a job checks out.
//
// Parse accepts only requests it fully understands. Anything else is not
// cacheable and goes to GitHub: an unknown line may change the response, and
// a request naming a ref, as want-ref does, has an answer that moves.
package gitcache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// keyVersion changes what every key hashes, and so retires every entry.
const keyVersion = "1"

// Pkt-line control packets.
const (
	pktFlush = "0000"
	pktDelim = "0001"
)

// maxPkt is the largest pkt-line the protocol allows, its header included.
const maxPkt = 65520

// ErrNotCacheable is wrapped by every reason a request is not cached.
var ErrNotCacheable = errors.New("not cacheable")

func notCacheable(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrNotCacheable}, args...)...)
}

// Fetch is a protocol v2 fetch for a client with no objects yet. What it leaves
// out, the client's agent, session and progress preference, does not change
// which objects the response holds.
type Fetch struct {
	ObjectFormat string
	Wants        []string // sorted, without repeats
	Deepen       int      // 0 is the full history
	Filter       string
	ThinPack     bool
	OfsDelta     bool
	IncludeTag   bool
}

// Parse reads the body of a git-upload-pack POST, already decompressed.
func Parse(body []byte) (Fetch, error) {
	caps, args, err := split(body)
	if err != nil {
		return Fetch{}, err
	}
	f := Fetch{ObjectFormat: "sha1"}
	command := false
	for _, l := range caps {
		switch {
		case l == "command=fetch":
			command = true
		case strings.HasPrefix(l, "command="):
			return Fetch{}, notCacheable("%s", l)
		case strings.HasPrefix(l, "agent="), strings.HasPrefix(l, "session-id="):
		case strings.HasPrefix(l, "object-format="):
			f.ObjectFormat = strings.TrimPrefix(l, "object-format=")
		default:
			return Fetch{}, notCacheable("capability %q", l)
		}
	}
	if !command {
		return Fetch{}, notCacheable("no command")
	}
	hexLen := map[string]int{"sha1": 40, "sha256": 64}[f.ObjectFormat]
	if hexLen == 0 {
		return Fetch{}, notCacheable("object format %q", f.ObjectFormat)
	}
	done := false
	for _, l := range args {
		name, value, _ := strings.Cut(l, " ")
		switch name {
		case "want":
			if !isOID(value, hexLen) {
				return Fetch{}, notCacheable("want %q", value)
			}
			f.Wants = append(f.Wants, value)
		case "deepen":
			n, err := strconv.Atoi(value)
			if err != nil || n <= 0 || f.Deepen != 0 {
				return Fetch{}, notCacheable("deepen %q", value)
			}
			f.Deepen = n
		case "filter":
			if value == "" || f.Filter != "" {
				return Fetch{}, notCacheable("filter %q", value)
			}
			f.Filter = value
		case "done":
			done = true
		case "no-progress":
		case "thin-pack":
			f.ThinPack = true
		case "ofs-delta":
			f.OfsDelta = true
		case "include-tag":
			f.IncludeTag = true
		default:
			return Fetch{}, notCacheable("argument %q", name)
		}
	}
	if !done {
		return Fetch{}, notCacheable("negotiation is not finished")
	}
	if len(f.Wants) == 0 {
		return Fetch{}, notCacheable("no wants")
	}
	slices.Sort(f.Wants)
	f.Wants = slices.Compact(f.Wants)
	return f, nil
}

// split reads pkt-lines into the capabilities before the delimiter and the
// arguments after it, and insists the request ends at the flush.
func split(body []byte) (caps, args []string, err error) {
	seenDelim := false
	for rest := body; ; {
		if len(rest) < 4 {
			return nil, nil, notCacheable("truncated request")
		}
		header := string(rest[:4])
		switch header {
		case pktFlush:
			if len(rest) != 4 || !seenDelim {
				return nil, nil, notCacheable("malformed request")
			}
			return caps, args, nil
		case pktDelim:
			if seenDelim {
				return nil, nil, notCacheable("malformed request")
			}
			seenDelim = true
			rest = rest[4:]
			continue
		}
		n, perr := strconv.ParseUint(header, 16, 16)
		if perr != nil || n <= 4 || n > maxPkt || int(n) > len(rest) {
			return nil, nil, notCacheable("bad pkt-line")
		}
		line := strings.TrimSuffix(string(rest[4:n]), "\n")
		if seenDelim {
			args = append(args, line)
		} else {
			caps = append(caps, line)
		}
		rest = rest[n:]
	}
}

func isOID(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range []byte(s) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Key names this fetch of repository (see Repository) in the cache.
func (f Fetch) Key(repository string) string {
	h := sha256.New()
	for _, part := range [][]string{
		{keyVersion, repository, f.ObjectFormat, strconv.Itoa(f.Deepen), f.Filter},
		{strconv.FormatBool(f.ThinPack), strconv.FormatBool(f.OfsDelta), strconv.FormatBool(f.IncludeTag)},
		f.Wants,
	} {
		for _, s := range part {
			// The length keeps one field from running into the next.
			fmt.Fprintf(h, "%d:%s,", len(s), s)
		}
		h.Write([]byte{'\n'})
	}
	return "git/" + hex.EncodeToString(h.Sum(nil))
}

// Repository returns the lower-case owner/name of a git-upload-pack request's
// URL path, such as /Owner/Repo.git/git-upload-pack. GitHub's names ignore
// case, so one repository has one set of keys.
func Repository(path string) (string, bool) {
	rest, ok := strings.CutSuffix(path, "/git-upload-pack")
	if !ok {
		return "", false
	}
	rest = strings.TrimSuffix(strings.TrimPrefix(rest, "/"), ".git")
	owner, name, ok := strings.Cut(rest, "/")
	if !ok || !validName(owner) || !validName(name) {
		return "", false
	}
	return strings.ToLower(owner + "/" + name), true
}

func validName(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, c := range []byte(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}
