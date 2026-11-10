package provisioner

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// consoleDirCutNote ends a console log that stopped because all runner logs
// together reached the directory budget.
const consoleDirCutNote = "\n[firerunner: runner logs reached --log-dir-max-mib; the rest was dropped]\n"

// cutNoteMax is the room a log needs for whichever closing note it ends with.
var cutNoteMax = int64(max(len(consoleCutNote), len(consoleDirCutNote)))

// logBudget holds every per-runner console log in a directory to one byte
// budget, however many guests write at once. One lock covers the bytes the
// live logs have written, what each of them may still need for its closing
// note, and the logs already closed; a write that the budget cannot take
// first removes the oldest closed logs, then is cut short. It never removes
// a log that is still open: an unlinked file that is still being written
// keeps taking disk that no directory listing shows.
type logBudget struct {
	dir     string
	perFile int64 // 0 = no per-file cap
	max     int64
	log     *slog.Logger

	mu sync.Mutex
	// live holds the open logs, by path.
	live map[string]*budgetedLog
	// liveBytes is what the open logs have written; reserved is the room
	// kept for the closing note of each open log that is not yet cut.
	liveBytes, reserved int64
	// closed is the size of the closed logs, as of the last directory scan
	// plus the logs closed since. It only decides whether a scan could
	// free anything.
	closed int64
}

func newLogBudget(dir string, perFile, dirMax int64, log *slog.Logger) *logBudget {
	return &logBudget{dir: dir, perFile: perFile, max: dirMax, log: log, live: map[string]*budgetedLog{}}
}

// open creates (or truncates) name's log and returns its writer, then
// removes the oldest closed logs until the directory has room for one more
// full log, as a single runner always had. A name already open is refused:
// truncating it would lose bytes the budget still counts.
func (b *logBudget) open(name string) (*budgetedLog, error) {
	path := filepath.Join(b.dir, name+".log")
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.live[path]; ok {
		return nil, fmt.Errorf("console log %s is already open", path)
	}
	// 0600: the console can carry whatever a job printed to it.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	l := &budgetedLog{b: b, path: path, file: file, left: b.perFile}
	b.live[path] = l
	b.pruneLocked(b.max-b.perFile, true)
	if b.inUseLocked()+cutNoteMax > b.max {
		b.pruneLocked(b.max-cutNoteMax, false)
	}
	if b.inUseLocked()+cutNoteMax <= b.max {
		b.reserved += cutNoteMax
	} else {
		// No room even for a closing note: the log stays empty.
		l.cut = true
	}
	return l, nil
}

// inUseLocked is what the logs take, or may take for their closing notes.
func (b *logBudget) inUseLocked() int64 { return b.closed + b.liveBytes + b.reserved }

// pruneLocked removes the oldest closed logs until everything in use takes at
// most target bytes. Unless told to rescan, it skips the directory scan when
// nothing needs to go or no closed log is left. It is best-effort: a failure
// is logged.
func (b *logBudget) pruneLocked(target int64, rescan bool) {
	if !rescan && (b.inUseLocked() <= target || b.closed <= 0) {
		return
	}
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		b.log.Warn("list runner logs", "dir", b.dir, "err", err)
		return
	}
	type logFile struct {
		path string
		size int64
		mod  time.Time
	}
	var logs []logFile
	b.closed = 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		path := filepath.Join(b.dir, e.Name())
		if _, open := b.live[path]; open {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		logs = append(logs, logFile{path, info.Size(), info.ModTime()})
		b.closed += info.Size()
	}
	sort.Slice(logs, func(i, j int) bool { return logs[i].mod.Before(logs[j].mod) })
	for _, l := range logs {
		if b.inUseLocked() <= target {
			return
		}
		if err := os.Remove(l.path); err != nil {
			b.log.Warn("remove old runner log", "path", l.path, "err", err)
			continue
		}
		b.closed -= l.size
	}
}

// budgetedLog is one runner's console log under a logBudget.
type budgetedLog struct {
	b    *logBudget
	path string
	// mu serialises this log's writes, so its bytes and closing note land in
	// order; the budget's lock is held only to account for them.
	mu      sync.Mutex
	file    *os.File
	written int64 // guarded by b.mu
	left    int64 // per-file bytes left; unused when b.perFile is 0
	cut     bool  // guarded by b.mu
	closed  bool  // guarded by b.mu
}

// Write takes what the per-file cap and the shared budget allow, then the
// closing note once, then nothing. It always reports a full write: losing
// log bytes must not stop the console copy.
func (l *budgetedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.b
	b.mu.Lock()
	if l.cut || l.closed {
		b.mu.Unlock()
		return len(p), nil
	}
	want := int64(len(p))
	note := ""
	if b.perFile > 0 && want > l.left {
		want, note = l.left, consoleCutNote
	}
	if room := b.max - b.inUseLocked(); want > room {
		b.pruneLocked(b.max-want, false)
		if room = b.max - b.inUseLocked(); want > room {
			want, note = max(room, 0), consoleDirCutNote
		}
	}
	l.left -= want
	l.written += want
	b.liveBytes += want
	if note != "" {
		// The note takes the room reserved for it at open.
		l.cut = true
		b.reserved -= cutNoteMax
		l.written += int64(len(note))
		b.liveBytes += int64(len(note))
	}
	b.mu.Unlock()

	// The file write happens outside the budget's lock; bytes the file did
	// not take are given back.
	short := int64(0)
	if want > 0 {
		n, _ := l.file.Write(p[:want])
		short += want - int64(n)
	}
	if note != "" {
		n, _ := io.WriteString(l.file, note)
		short += int64(len(note) - n)
	}
	if short > 0 {
		b.mu.Lock()
		if !l.closed {
			l.written -= short
			b.liveBytes -= short
		}
		b.mu.Unlock()
	}
	return len(p), nil
}

// Close closes the file and hands its bytes from the live logs to the
// closed ones, which pruning may then remove.
func (l *budgetedLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.b
	b.mu.Lock()
	if l.closed {
		b.mu.Unlock()
		return nil
	}
	l.closed = true
	delete(b.live, l.path)
	b.liveBytes -= l.written
	b.closed += l.written
	if !l.cut {
		b.reserved -= cutNoteMax
	}
	b.mu.Unlock()
	return l.file.Close()
}
