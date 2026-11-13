package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/firerunnerhq/firerunner/proprietary/jobstore"
)

const usageUsage = `usage:
  firerunner usage [--after CURSOR] [--limit N] [--env-file FILE|none]
      print the usage events after CURSOR, oldest first

One JSON object a line: {"cursor": "...", "event": {...}}. An event is one
job a runner ran, written when the job completed: its tenant, repository,
workflow, tier, machine, and its run time in duration_ms. Its fields are
described by proprietary/jobstore/usage-event.schema.json.

To read them all, pass the last line's cursor as the next --after until a
read prints fewer than --limit (default 1000, at most 10000) lines. An event
may be read again, after a lost cursor, say: count each event_id once.

The events are in the daemon's job store, FR_JOB_EVENTS_DSN or the file
FR_JOB_EVENTS_DSN_FILE names; with no FR_* variable set, the daemon's
settings file is read for them, as status and doctor do (--env-file FILE
reads another, --env-file none reads none).
exit status: 0 done, 1 failed, 2 usage error
`

// usageReader is what usageCmd reads from; a *jobstore.UsageReader.
type usageReader interface {
	Read(ctx context.Context, after jobstore.Cursor, limit int) ([]jobstore.UsageEntry, error)
	Close()
}

// usageCmd runs "firerunner usage" and returns the exit status. open
// connects to the job store a URL names.
func usageCmd(args []string, stdout, stderr io.Writer, open func(dsn string) (usageReader, error)) int {
	usageErr := func(format string, a ...any) int {
		fmt.Fprintf(stderr, format+"\n%s", append(a, usageUsage)...)
		return 2
	}
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "-help" || a == "help" {
			fmt.Fprint(stdout, usageUsage)
			return 0
		}
	}
	rest, err := diagEnv("usage", args, stderr)
	if err != nil {
		return usageErr("firerunner usage: %v", err)
	}
	var after jobstore.Cursor
	limit := 1000
	for i := 0; i < len(rest); i++ {
		name, val, hasVal := strings.Cut(rest[i], "=")
		name = "--" + strings.TrimLeft(name, "-")
		switch name {
		case "--after", "--limit":
		default:
			return usageErr("firerunner usage: unknown argument %q", rest[i])
		}
		if !hasVal {
			if i+1 >= len(rest) {
				return usageErr("%s needs a value", name)
			}
			i++
			val = rest[i]
		}
		switch name {
		case "--after":
			if after, err = jobstore.ParseCursor(val); err != nil {
				return usageErr("--after: %v", err)
			}
		case "--limit":
			if limit, err = strconv.Atoi(val); err != nil || limit < 1 || limit > 10000 {
				return usageErr("--limit %q: give a number from 1 to 10000", val)
			}
		}
	}
	dsn := os.Getenv("FR_JOB_EVENTS_DSN")
	if file := os.Getenv("FR_JOB_EVENTS_DSN_FILE"); dsn == "" && file != "" {
		raw, err := os.ReadFile(file)
		if err != nil {
			fmt.Fprintf(stderr, "firerunner usage: cannot read FR_JOB_EVENTS_DSN_FILE: %v\n", err)
			return 1
		}
		dsn = strings.TrimSpace(string(raw))
	}
	if dsn == "" {
		return usageErr("firerunner usage: no job store: set FR_JOB_EVENTS_DSN or FR_JOB_EVENTS_DSN_FILE as the daemon's")
	}
	r, err := open(dsn)
	if err != nil {
		fmt.Fprintf(stderr, "firerunner usage: %v\n", err)
		return 1
	}
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	entries, err := r.Read(ctx, after, limit)
	if err != nil {
		fmt.Fprintf(stderr, "firerunner usage: %v\n", err)
		return 1
	}
	enc := json.NewEncoder(stdout)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			fmt.Fprintf(stderr, "firerunner usage: %v\n", err)
			return 1
		}
	}
	return 0
}

func openUsage(dsn string) (usageReader, error) {
	return jobstore.OpenUsage(dsn)
}
