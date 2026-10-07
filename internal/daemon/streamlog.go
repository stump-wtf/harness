package daemon

// Pipe-Run Streams In The Logs View
//
// A structured one-shot has no terminal (ADR-0033 "Structured one-shots run on
// pipes"), so its run log holds only the daemon's lifecycle lines and the
// agent's stderr. Its stdout is the run's stream file, <id>.stream.jsonl.
// `harness logs NAME --run N --raw` and `harness trigger --wait` read a run
// through opLogsRun, so for such a run the reply's text is the stream's tail,
// and once the run has ended the run log follows it, each under a tail-style
// header.
//
// The order is for trigger --wait, which prints only what extends the text it
// has already printed. The stream grows while the run is live, and the run log
// block is appended once, after the run ends, so every poll's text extends the
// last one's.
//
// A stream line can be megabytes and a protocol frame is capped at 16 MiB, so
// the tail is bounded twice: each line shown is cut at streamLineShown, with a
// note of what was cut, and the whole tail at streamTailBytes, oldest lines
// dropped first. The file on disk stays whole. It is read front to back
// holding one line at a time, which works for a compressed, sealed stream as
// well as a plain one.
//
// Governing: ADR-0033 "Structured one-shots run on pipes", "What reads the
// records" (`--raw` reads the durable log or the stream file); SPEC-0017
// REQ-18; SPEC-0008 REQ "Per-Run Logs"; ADR-0008 (readers mask too).
//
// @joestump-agent 09/28/2026 - Added for
// https://github.com/stump-wtf/harness/issues/18.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/stump-wtf/agent-trace/redact"
)

const (
	// streamLineShown is the most of one stream line the logs view prints.
	streamLineShown = 64 << 10
	// streamTailBytes bounds a stream tail, well inside a protocol frame.
	streamTailBytes = 8 << 20
)

// openRunArtifact opens a run artifact for reading. It is the one place a
// reader opens a stream file, so reading a sealed (compressed) one is a change
// here and nowhere else.
var openRunArtifact = func(path string) (io.ReadCloser, error) { return os.Open(path) }

// readStreamTail returns the last lines of a run's stream file, bounded and
// masked, and whether the file exists.
func readStreamTail(path string, lines int) (string, bool) {
	if path == "" {
		return "", false
	}
	f, err := openRunArtifact(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	tail := streamTail(f, lines, streamLineShown, streamTailBytes)
	// Written masked; masked again here, as redactTail does for logs, to
	// cover anything a rule added since. JSONLine is idempotent.
	var mask redact.Lines
	for i, ln := range tail {
		tail[i] = mask.JSONLine(ln)
	}
	if len(tail) == 0 {
		return "", true
	}
	return strings.Join(tail, "\n") + "\n", true
}

// streamTail reads r to its end and returns its last n lines (every line when
// n <= 0), each cut at lineMax bytes, dropping the oldest while the total
// exceeds budget. It holds at most one line and the kept tail at a time.
func streamTail(r io.Reader, n, lineMax, budget int) []string {
	br := bufio.NewReaderSize(r, 64<<10)
	var tail []string
	size := 0
	for {
		kept, total, err := nextLineCapped(br, lineMax)
		if total > 0 || err == nil {
			s := string(kept)
			if total > len(kept) {
				s += fmt.Sprintf(" …[%d more bytes; the whole line is in the stream file]", total-len(kept))
			}
			tail = append(tail, s)
			size += len(s)
			for len(tail) > 1 && ((n > 0 && len(tail) > n) || size > budget) {
				size -= len(tail[0])
				tail[0] = ""
				tail = tail[1:]
			}
		}
		if err != nil {
			return tail
		}
	}
}

// nextLineCapped reads one line, keeping at most max bytes of it, and reports
// its full length. err is set at the end of the stream, where kept is the
// final line if it had no newline.
func nextLineCapped(br *bufio.Reader, max int) (kept []byte, total int, err error) {
	for {
		frag, e := br.ReadSlice('\n')
		end := e == nil
		if end {
			frag = frag[:len(frag)-1]
		}
		total += len(frag)
		if room := max - len(kept); room > 0 {
			kept = append(kept, frag[:min(room, len(frag))]...)
		}
		if end {
			return kept, total, nil
		}
		if !errors.Is(e, bufio.ErrBufferFull) {
			return kept, total, e
		}
	}
}

// pipeRunText is what the logs view shows of a pipe run: its stream and, once
// the run has ended, its run log after it.
func pipeRunText(streamPath, stream, logPath, runLog string, ended bool) string {
	text := "==> " + streamPath + " <==\n" + stream
	if !ended || runLog == "" {
		return text
	}
	return text + "\n==> " + logPath + " <==\n" + runLog
}
