package nvidia

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/minhhhduc/gnvipi/internal/captcha"
)

// frameSize is the number of non-empty records per frame; frame 1 is newest.
const frameSize = 200

// StatsLogOptions bounds persisted history and the rate of uncached frame reads.
// Limits include the active file and every retained segment, in bytes.
type StatsLogOptions struct {
	MaxBytes       int64
	SegmentBytes   int64
	RotateInterval time.Duration
	Retention      time.Duration
	ReadInterval   time.Duration
}

func DefaultStatsLogOptions() StatsLogOptions {
	return StatsLogOptions{
		MaxBytes: 16 << 20, SegmentBytes: 4 << 20,
		RotateInterval: 24 * time.Hour, Retention: 7 * 24 * time.Hour,
		ReadInterval: time.Second,
	}
}

func (o StatsLogOptions) validate() error {
	if o.MaxBytes <= 0 || o.SegmentBytes <= 0 || o.SegmentBytes > o.MaxBytes {
		return errors.New("stats log: require 0 < segment bytes <= max bytes")
	}
	// Counts and scanner lengths use int, and include a trailing newline.
	if o.MaxBytes >= int64(int(^uint(0)>>1)) {
		return errors.New("stats log: byte limit exceeds platform limit")
	}
	if o.RotateInterval <= 0 || o.Retention <= 0 || o.ReadInterval <= 0 {
		return errors.New("stats log: rotation, retention and read intervals must be positive")
	}
	if o.ReadInterval > time.Duration(1<<31-1)*time.Millisecond {
		return errors.New("stats log: read interval exceeds the browser timer limit (2147483647ms)")
	}
	return nil
}

// StatsLogFile is the active NDJSON file. Closed segments use numeric suffixes.
// Override with GNVIPI_STATS_FILE.
func StatsLogFile() string {
	if p := os.Getenv("GNVIPI_STATS_FILE"); p != "" {
		return p
	}
	return filepath.Join(filepath.Dir(captcha.ChampionStateFile()), "stats.jsonl")
}

// ErrStatsReadLimited indicates a different frame was requested before the
// configured read interval elapsed. Repeated polls of the same frame are cached.
var ErrStatsReadLimited = errors.New("stats log: history read rate exceeded")

type statsSegment struct {
	path              string
	size              int64
	created, modified time.Time
	lines             int
	// One seek checkpoint per 200 non-empty records, not one allocation per line.
	offsets []int64
	newline bool
}

type statsLog struct {
	mu          sync.Mutex
	path        string
	f           *os.File
	werr        bool
	opts        StatsLogOptions
	loaded      bool
	segments    []*statsSegment // oldest first; active file, if any, last
	sequence    uint64
	stop        chan struct{}
	cached      []RequestEvent // at most one frame
	cachedFrame int
	cachedTotal int
	readAt      time.Time
}

func openStatsLogAt(path string) *statsLog {
	return &statsLog{path: path, opts: DefaultStatsLogOptions()}
}

var (
	statLogOnce sync.Once
	statLog     *statsLog
)

func statsLogInstance() *statsLog {
	statLogOnce.Do(func() { statLog = openStatsLogAt(StatsLogFile()) })
	return statLog
}

// ConfigureStatsLog validates and applies bounds before serving. Existing logs
// are normalized immediately; missing files and directories remain absent.
func ConfigureStatsLog(opts StatsLogOptions) error { return statsLogInstance().configure(opts) }

// StatsReadInterval lets the dashboard use the same configured polling cadence.
func StatsReadInterval() time.Duration {
	l := statsLogInstance()
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.opts.ReadInterval
}

func (l *statsLog) configure(opts StatsLogOptions) error {
	if err := opts.validate(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.closeFile(); err != nil {
		return err
	}
	l.stopMaintenance()
	l.opts, l.loaded, l.segments = opts, false, nil
	l.invalidate()
	return l.load(time.Now())
}

func (l *statsLog) closeFile() error {
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

func (l *statsLog) stopMaintenance() {
	if l.stop != nil {
		close(l.stop)
		l.stop = nil
	}
}

// close releases all handles before Windows test directories are removed.
func (l *statsLog) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stopMaintenance()
	if err := l.closeFile(); err != nil {
		l.fail(err)
	}
}

// fail is always called under mu. Persistence errors never break request counters.
func (l *statsLog) fail(err error) {
	if !l.werr {
		l.werr = true
		log.Printf("stats log: %v", err)
	}
}

func (l *statsLog) invalidate() {
	l.cached = nil
	l.cachedFrame, l.cachedTotal = 0, 0
	// Keep readAt: eviction must not let rapid history switching bypass the limit.
}

func (l *statsLog) startMaintenance() {
	if l.stop != nil || len(l.segments) == 0 {
		return
	}
	interval := min(time.Minute, l.opts.Retention)
	stop := make(chan struct{})
	l.stop = stop
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-ticker.C:
				l.mu.Lock()
				// A stopped/reconfigured worker must not touch the replacement log.
				if l.stop == stop {
					if err := l.prune(now, 0); err != nil {
						l.fail(err)
					}
				}
				l.mu.Unlock()
			}
		}
	}()
}

// load discovers/indexes once per process (or explicit reconfiguration), after
// bounding legacy files. No frame poll rereads history to count records.
func (l *statsLog) load(now time.Time) error {
	if l.loaded {
		return nil
	}
	entries, err := os.ReadDir(filepath.Dir(l.path))
	if os.IsNotExist(err) {
		l.loaded = true
		return nil
	}
	if err != nil {
		return err
	}
	base := filepath.Base(l.path)
	type numbered struct {
		segment *statsSegment
		n       uint64
	}
	var found []numbered
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		var n uint64
		if name != base {
			if !strings.HasPrefix(name, base+".") {
				continue
			}
			n, err = strconv.ParseUint(strings.TrimPrefix(name, base+"."), 10, 64)
			if err != nil || n == 0 {
				continue
			}
			l.sequence = max(l.sequence, n)
		}
		fi, err := entry.Info()
		if err != nil {
			return err
		}
		found = append(found, numbered{&statsSegment{
			path: filepath.Join(filepath.Dir(l.path), name), size: fi.Size(),
			created: fi.ModTime(), modified: fi.ModTime(),
		}, n})
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].n == 0 {
			return false
		}
		if found[j].n == 0 {
			return true
		}
		return found[i].n < found[j].n
	})
	l.segments = nil
	for _, item := range found {
		l.segments = append(l.segments, item.segment)
	}
	for _, s := range l.segments {
		if s.size > l.opts.SegmentBytes {
			if err := trimStatsSegment(s, l.opts.SegmentBytes); err != nil {
				return err
			}
			log.Printf("stats log: trimmed oversized segment %s to %d bytes", s.path, s.size)
		}
	}
	if err := l.prune(now, 0); err != nil {
		return err
	}
	for _, s := range l.segments {
		if err := indexStatsSegment(s); err != nil {
			return err
		}
		if s.path == l.path {
			// Portable modification times cannot recover a segment's birth time.
			// Seal recovered active files on the next append rather than extend
			// their rotation lifetime on every restart; no sidecar is needed.
			s.created = time.Time{}
		}
	}
	l.loaded = true
	l.startMaintenance()
	return nil
}

// trimStatsSegment keeps a complete newest suffix, in place. SectionReader uses
// ReadAt, so copying backwards to offset zero cannot overwrite unread bytes and
// needs neither a second cap-sized file nor an open-file rename on Windows.
func trimStatsSegment(s *statsSegment, limit int64) (err error) {
	f, err := os.OpenFile(s.path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	start := s.size - limit
	r := bufio.NewReader(io.NewSectionReader(f, start-1, limit+1))
	for {
		chunk, readErr := r.ReadSlice('\n')
		start += int64(len(chunk)) - 1
		if readErr != nil && !errors.Is(readErr, bufio.ErrBufferFull) && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if !errors.Is(readErr, bufio.ErrBufferFull) {
			break
		}
		start++
	}
	start = min(start, s.size)
	if _, err := io.CopyBuffer(f, io.NewSectionReader(f, start, s.size-start), make([]byte, 32<<10)); err != nil {
		return err
	}
	if err := f.Truncate(s.size - start); err != nil {
		return err
	}
	s.size -= start
	return nil
}

// scanStatsLines streams even arbitrarily long corrupt lines with a fixed read
// buffer. It retains only a checkpoint and length, never the full line contents.
func scanStatsLines(r io.Reader, offset int64, visit func(int64, int64)) error {
	reader := bufio.NewReaderSize(r, 32<<10)
	start, length, nonempty := offset, int64(0), false
	for {
		chunk, err := reader.ReadSlice('\n')
		length += int64(len(chunk))
		nonempty = nonempty || len(bytes.TrimSpace(chunk)) != 0
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if nonempty {
			visit(start, length)
		}
		start += length
		length, nonempty = 0, false
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func indexStatsSegment(s *statsSegment) (err error) {
	f, err := os.Open(s.path)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	s.lines, s.offsets = 0, nil
	err = scanStatsLines(f, 0, func(offset, _ int64) {
		if s.lines%frameSize == 0 {
			s.offsets = append(s.offsets, offset)
		}
		s.lines++
	})
	if err != nil {
		return err
	}
	if s.size > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], s.size-1); err != nil {
			return err
		}
		s.newline = last[0] == '\n'
	}
	return nil
}

func (l *statsLog) prune(now time.Time, reserve int64) error {
	var total int64
	for _, s := range l.segments {
		total += s.size
	}
	for i := 0; i < len(l.segments); {
		s := l.segments[i]
		if s.size > 0 && now.Sub(s.modified) < l.opts.Retention && total <= l.opts.MaxBytes-reserve {
			i++
			continue
		}
		if s.path == l.path {
			if err := l.closeFile(); err != nil {
				return err
			}
		}
		if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("stats log: cannot enforce disk bounds removing %s: %w", s.path, err)
		}
		total -= s.size
		copy(l.segments[i:], l.segments[i+1:])
		l.segments[len(l.segments)-1] = nil
		l.segments = l.segments[:len(l.segments)-1]
		l.invalidate()
	}
	return nil
}

func (l *statsLog) append(ev *RequestEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if err := l.load(now); err != nil {
		l.fail(err)
		return
	}
	if err := l.prune(now, 0); err != nil {
		l.fail(err)
		return
	}
	// Avoid encoding an obviously oversized model. Actual encoded size (including
	// JSON escaping) is checked too; a single record can never exceed a segment.
	if int64(len(ev.Model)) > l.opts.SegmentBytes {
		l.fail(errors.New("record exceeds stats segment byte limit"))
		return
	}
	line, err := json.Marshal(ev)
	if err != nil {
		l.fail(err)
		return
	}
	line = append(line, '\n')
	if int64(len(line)) > l.opts.SegmentBytes {
		l.fail(errors.New("record exceeds stats segment byte limit"))
		return
	}
	var active *statsSegment
	if len(l.segments) > 0 && l.segments[len(l.segments)-1].path == l.path {
		active = l.segments[len(l.segments)-1]
		separator := int64(0)
		if active.size > 0 && !active.newline {
			separator = 1
		}
		if active.size+separator+int64(len(line)) > l.opts.SegmentBytes || now.Sub(active.created) >= l.opts.RotateInterval {
			if err := l.closeFile(); err != nil {
				l.fail(err)
				return
			}
			if l.sequence == ^uint64(0) {
				l.fail(errors.New("stats segment sequence exhausted"))
				return
			}
			l.sequence++
			path := fmt.Sprintf("%s.%020d", l.path, l.sequence)
			if err := os.Rename(l.path, path); err != nil {
				l.fail(err)
				return
			}
			active.path = path
			active = nil
		}
	}
	reserve := int64(len(line))
	if active != nil && active.size > 0 && !active.newline {
		reserve++
	}
	if err := l.prune(now, reserve); err != nil {
		l.fail(err)
		return
	}
	if active == nil {
		if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
			l.fail(err)
			return
		}
		active = &statsSegment{path: l.path, created: now, modified: now}
		l.segments = append(l.segments, active)
	}
	if l.f == nil {
		l.f, err = os.OpenFile(l.path, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			l.fail(err)
			return
		}
	}
	oldSize := active.size
	if _, err := l.f.Seek(oldSize, io.SeekStart); err != nil {
		l.fail(err)
		return
	}
	if reserve > int64(len(line)) {
		if _, err := l.f.Write([]byte{'\n'}); err != nil {
			l.fail(err)
			return
		}
	}
	written, err := l.f.Write(line)
	if err != nil || written != len(line) {
		if err == nil {
			err = io.ErrShortWrite
		}
		l.fail(err)
		if err := l.f.Truncate(oldSize); err != nil {
			l.fail(err)
		}
		return
	}
	if active.lines%frameSize == 0 {
		active.offsets = append(active.offsets, oldSize+reserve-int64(len(line)))
	}
	active.lines++
	active.size += reserve
	active.modified, active.newline = now, true
	l.startMaintenance()
}

func (l *statsLog) count() int {
	var total int
	for _, s := range l.segments {
		total += s.lines
	}
	return total
}

func (l *statsLog) framesCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if err := l.load(now); err != nil {
		l.fail(err)
		return 0
	}
	if err := l.prune(now, 0); err != nil {
		l.fail(err)
		return 0
	}
	return (l.count() + frameSize - 1) / frameSize
}

// readStatsRange seeks to the nearest sparse checkpoint and scans at most one
// frame plus 199 preceding non-empty records. Handles close before rotation.
func readStatsRange(s *statsSegment, lo, hi int, limit int64) (out []RequestEvent, err error) {
	f, err := os.Open(s.path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	block := lo / frameSize
	if _, err := f.Seek(s.offsets[block], io.SeekStart); err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, min(int(limit)+1, 32<<10)), int(limit)+1)
	index := block * frameSize
	for index < hi && scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		if index >= lo {
			var ev RequestEvent
			if json.Unmarshal(scanner.Bytes(), &ev) == nil {
				out = append(out, ev)
			}
		}
		index++
	}
	return out, scanner.Err()
}

func (l *statsLog) readFrame(n int) ([]RequestEvent, int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if err := l.load(now); err != nil {
		l.fail(err)
		return nil, 0, err
	}
	if err := l.prune(now, 0); err != nil {
		l.fail(err)
		return nil, 0, err
	}
	count := l.count()
	total := (count + frameSize - 1) / frameSize
	if n < 1 || n > total {
		return nil, total, fmt.Errorf("stats log: frame %d out of range 1-%d", n, total)
	}
	if time.Since(l.readAt) < l.opts.ReadInterval {
		if l.cachedFrame == n {
			return append([]RequestEvent{}, l.cached...), l.cachedTotal, nil
		}
		return nil, total, ErrStatsReadLimited
	}
	l.readAt = time.Now()
	hi := count - (n-1)*frameSize
	lo := max(0, hi-frameSize)
	out := make([]RequestEvent, 0, hi-lo)
	base := 0
	for _, s := range l.segments {
		end := base + s.lines
		if end > lo && base < hi {
			events, err := readStatsRange(s, max(0, lo-base), min(s.lines, hi-base), l.opts.SegmentBytes)
			if err != nil {
				l.fail(err)
				return nil, total, err
			}
			out = append(out, events...)
		}
		base = end
		if base >= hi {
			break
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	l.cached, l.cachedFrame, l.cachedTotal = out, n, total
	return append([]RequestEvent{}, out...), total, nil
}

func AppendLog(ev *RequestEvent)                   { statsLogInstance().append(ev) }
func FramesCount() int                             { return statsLogInstance().framesCount() }
func ReadFrame(n int) ([]RequestEvent, int, error) { return statsLogInstance().readFrame(n) }
