package nvidia

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"glm52-nvidia/internal/captcha"
)

// frameSize is how many request events make up one log frame. Frames are
// numbered 1-based in time order over the whole append-only file.
const frameSize = 200

// StatsLogFile is where request events get appended as NDJSON, one JSON
// object per line. Override with GLM52_STATS_FILE.
func StatsLogFile() string {
	if p := os.Getenv("GLM52_STATS_FILE"); p != "" {
		return p
	}
	return filepath.Join(filepath.Dir(captcha.ChampionStateFile()), "stats.jsonl")
}

// statsLog is an append-only NDJSON log, opened lazily on first write so a
// read-only process never creates the file.
type statsLog struct {
	mu   sync.Mutex
	path string
	f    *os.File
	werr bool // write errors are logged once, never fatal
}

func openStatsLogAt(path string) *statsLog {
	return &statsLog{path: path}
}

var (
	statLogOnce sync.Once
	statLog     *statsLog
)

func statsLogInstance() *statsLog {
	statLogOnce.Do(func() { statLog = openStatsLogAt(StatsLogFile()) })
	return statLog
}

// close releases the append handle (tests clean up on Windows).
func (l *statsLog) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
}

// fail logs the first error and swallows the rest — stats must never break
// the request path.
func (l *statsLog) fail(err error) {
	if !l.werr {
		l.werr = true
		log.Printf("stats log: %v", err)
	}
}

func (l *statsLog) append(ev RequestEvent) {
	line, err := json.Marshal(ev)
	if err != nil {
		l.fail(err)
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
			l.fail(err)
			return
		}
		f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			l.fail(err)
			return
		}
		l.f = f
	}
	if _, err := l.f.Write(append(line, '\n')); err != nil {
		l.fail(err)
	}
}

// lines returns the log's non-empty NDJSON lines in write order.
// ponytail: full scan, index sidecar if the file ever gets big
func (l *statsLog) lines() [][]byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	raw, err := os.ReadFile(l.path)
	if err != nil {
		return nil
	}
	var out [][]byte
	for _, ln := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(ln)) > 0 {
			out = append(out, ln)
		}
	}
	return out
}

// framesCount is the number of frames on disk, partial last frame included.
func (l *statsLog) framesCount() int {
	n := len(l.lines())
	return (n + frameSize - 1) / frameSize
}

// readFrame returns frame n (1-based) and the total frame count.
func (l *statsLog) readFrame(n int) ([]RequestEvent, int, error) {
	ls := l.lines()
	total := (len(ls) + frameSize - 1) / frameSize
	if n < 1 || n > total {
		return nil, total, fmt.Errorf("stats log: frame %d out of range 1-%d", n, total)
	}
	lo, hi := (n-1)*frameSize, n*frameSize
	if hi > len(ls) {
		hi = len(ls)
	}
	out := make([]RequestEvent, 0, hi-lo)
	for _, ln := range ls[lo:hi] {
		var ev RequestEvent
		if err := json.Unmarshal(ln, &ev); err != nil {
			l.fail(err)
			continue
		}
		out = append(out, ev)
	}
	return out, total, nil
}

// AppendLog writes one request event to the stats log.
func AppendLog(ev RequestEvent) { statsLogInstance().append(ev) }

// FramesCount is the number of frames on disk, partial last frame included.
func FramesCount() int { return statsLogInstance().framesCount() }

// ReadFrame returns frame n (1-based) and the total frame count.
func ReadFrame(n int) ([]RequestEvent, int, error) { return statsLogInstance().readFrame(n) }
