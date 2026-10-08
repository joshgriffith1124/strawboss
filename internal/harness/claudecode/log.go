package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/joshgriffith1124/strawboss/internal/harness"
)

// The worker log: one JSON record per line, written by the harness as the
// claude stream arrives and tailed by the TUI (the observability surface,
// like a dsh session log). Never read by the supervisor.
//
//	{"ts":…,"kind":"tool","text":"Bash go test ./..."}
//	{"ts":…,"usage":{"in":…,"cache_read":…,"out":…},"ctx":…}
//	{"ts":…,"done":true,"status":"done","cost_usd":…}
type record struct {
	TS     time.Time `json:"ts"`
	Kind   string    `json:"kind,omitempty"`
	Text   string    `json:"text,omitempty"`
	Usage  *usageRec `json:"usage,omitempty"`
	Ctx    int       `json:"ctx,omitempty"`
	Done   bool      `json:"done,omitempty"`
	Status string    `json:"status,omitempty"`
	Cost   float64   `json:"cost_usd,omitempty"`
}

type usageRec struct {
	In        int `json:"in"`
	CacheRead int `json:"cache_read"`
	Out       int `json:"out"`
}

// LogPath is a claude worker's log: <stateDir>/logs/<workerID>.jsonl. The
// worker id is what the registry records as the session, so the TUI finds
// the log from the spawned event alone.
func LogPath(stateDir, workerID string) string {
	return filepath.Join(stateDir, "logs", workerID+".jsonl")
}

// TailItem is one worker-log record for the TUI. Replay marks records
// already in the file when the tail began (history after a restart).
type TailItem struct {
	Event  *harness.Event
	Usage  *harness.Usage
	Ctx    int
	Done   bool
	Replay bool
}

// Tail follows a worker log until its done record or ctx ends, waiting for
// the file to appear. A partial last line is retried from the same offset.
func Tail(ctx context.Context, path string) <-chan TailItem {
	out := make(chan TailItem, 64)
	go func() {
		defer close(out)
		var f *os.File
		for f == nil {
			var err error
			if f, err = os.Open(path); err == nil {
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
		defer f.Close()
		// Everything already in the file at open time is history.
		var replayEnd int64
		if fi, err := f.Stat(); err == nil {
			replayEnd = fi.Size()
		}
		var offset int64
		rd := bufio.NewReader(f)
		for {
			line, err := rd.ReadBytes('\n')
			if err == io.EOF || (err == nil && len(line) > 0 && line[len(line)-1] != '\n') {
				// Partial or no line yet: rewind to the line start and wait.
				if _, serr := f.Seek(offset, io.SeekStart); serr != nil {
					return
				}
				rd.Reset(f)
				select {
				case <-ctx.Done():
					return
				case <-time.After(300 * time.Millisecond):
				}
				continue
			}
			if err != nil {
				return
			}
			offset += int64(len(line))
			var r record
			if json.Unmarshal(line, &r) != nil {
				continue
			}
			it := TailItem{Replay: offset <= replayEnd}
			switch {
			case r.Done:
				it.Done = true
			case r.Usage != nil:
				it.Usage = &harness.Usage{InputTokens: r.Usage.In, CacheReadTokens: r.Usage.CacheRead, OutputTokens: r.Usage.Out}
				it.Ctx = r.Ctx
			case r.Kind != "":
				it.Event = &harness.Event{Time: r.TS, Kind: r.Kind, Text: r.Text}
			default:
				continue
			}
			select {
			case out <- it:
			case <-ctx.Done():
				return
			}
			if it.Done {
				return
			}
		}
	}()
	return out
}
