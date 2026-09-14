package nvidia

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// coalesceSSEEvents reads upstream SSE and yields coalesced event lines
// (without the trailing blank line). When window <= 0, lines are passed through.
func coalesceSSEEvents(src io.Reader, window time.Duration, emit func(line string) error) error {
	if window <= 0 {
		return pipeSSELines(src, emit)
	}

	type readResult struct {
		line string
		err  error
	}
	ch := make(chan readResult, 16)
	done := make(chan struct{})
	defer close(done)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				select {
				case ch <- readResult{err: fmt.Errorf("sse reader panicked: %v", r)}:
				case <-done:
				}
			}
		}()
		reader := bufio.NewReaderSize(src, 64<<10)
		for {
			line, err := reader.ReadString('\n')
			if len(line) > 0 {
				select {
				case ch <- readResult{line: strings.TrimRight(line, "\r\n")}:
				case <-done:
					return
				}
			}
			if err != nil {
				select {
				case ch <- readResult{err: err}:
				case <-done:
				}
				return
			}
		}
	}()

	var (
		pending   map[string]any
		timer     *time.Timer
		timerC    <-chan time.Time
		firstSent bool
	)
	stopTimer := func() {
		if timer != nil {
			timer.Stop()
			timer = nil
			timerC = nil
		}
	}
	defer stopTimer()
	flushPending := func() error {
		stopTimer()
		if pending == nil {
			return nil
		}
		raw, err := json.Marshal(pending)
		pending = nil
		if err != nil {
			return err
		}
		return emit("data: " + string(raw))
	}
	armTimer := func() {
		stopTimer()
		timer = time.NewTimer(window)
		timerC = timer.C
	}

	for {
		select {
		case <-timerC:
			if err := flushPending(); err != nil {
				return err
			}

		case rr := <-ch:
			if rr.err != nil && rr.line == "" {
				ferr := flushPending()
				if rr.err == io.EOF {
					return ferr
				}
				if ferr != nil {
					return ferr
				}
				return rr.err
			}

			line := rr.line
			switch {
			case line == "" || strings.HasPrefix(line, ":"):
				// ignore keep-alives / comments

			case line == "data: [DONE]":
				if err := flushPending(); err != nil {
					return err
				}
				if err := emit(line); err != nil {
					return err
				}

			case strings.HasPrefix(line, "data: "):
				data := strings.TrimPrefix(line, "data: ")
				if strings.TrimSpace(data) == "" {
					// Empty `data: \n\n` lines that NVIDIA emits during long
					// reasoning pauses — the OpenAI client treats any
					// forwarded `data:` line as a frame and may close on
					// a content-less one. Drop them, keep the connection
					// open, and let the real delta through when it lands.
					break
				}
				var chunk map[string]any
				if err := json.Unmarshal([]byte(data), &chunk); err != nil {
					if err := flushPending(); err != nil {
						return err
					}
					if err := emit(line); err != nil {
						return err
					}
					break
				}

				content, ok := mergeableContent(chunk)
				if !ok {
					if err := flushPending(); err != nil {
						return err
					}
					if err := emit(line); err != nil {
						return err
					}
					break
				}

				if pending == nil {
					pending = chunk
					// Flush the first content token immediately (TTFT), then
					// coalesce subsequent deltas within the window.
					if !firstSent {
						if err := flushPending(); err != nil {
							return err
						}
						firstSent = true
						break
					}
					armTimer()
					break
				}
				if tryMergeContent(pending, content) {
					break
				}
				if err := flushPending(); err != nil {
					return err
				}
				pending = chunk
				armTimer()

			default:
				if err := flushPending(); err != nil {
					return err
				}
				if err := emit(line); err != nil {
					return err
				}
			}

			if rr.err != nil {
				ferr := flushPending()
				if rr.err == io.EOF {
					return ferr
				}
				if ferr != nil {
					return ferr
				}
				return rr.err
			}
		}
	}
}

func pipeSSELines(src io.Reader, emit func(line string) error) error {
	reader := bufio.NewReaderSize(src, 64<<10)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			if trimmed != "" && !strings.HasPrefix(trimmed, ":") {
				// Same empty-data guard as the coalesce path: `data: \n\n`
				// from upstream during a long reasoning pause must not
				// reach the OpenAI consumer as a content-less frame.
				if strings.HasPrefix(trimmed, "data:") && strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")) == "" {
					if err := readerErrOrNil(err); err != nil {
						return err
					}
					continue
				}
				if errEmit := emit(trimmed); errEmit != nil {
					return errEmit
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func readerErrOrNil(err error) error {
	if err == nil || err == io.EOF {
		return nil
	}
	return err
}

// mergeableContent returns the delta content if this chunk is a pure content
// delta (safe to coalesce).
func mergeableContent(chunk map[string]any) (string, bool) {
	if u, ok := chunk["usage"]; ok && u != nil {
		return "", false
	}
	choices, ok := chunk["choices"].([]any)
	if !ok || len(choices) == 0 {
		return "", false
	}
	ch, ok := choices[0].(map[string]any)
	if !ok {
		return "", false
	}
	if fr, ok := ch["finish_reason"]; ok && fr != nil {
		if s, ok := fr.(string); ok && s != "" {
			return "", false
		}
	}
	delta, ok := ch["delta"].(map[string]any)
	if !ok {
		return "", false
	}
	content, hasContent := delta["content"].(string)
	if !hasContent || content == "" {
		return "", false
	}
	for k := range delta {
		switch k {
		case "content", "role":
		default:
			return "", false // reasoning_content, tool_calls, etc.
		}
	}
	return content, true
}

func tryMergeContent(pending map[string]any, content string) bool {
	choices, ok := pending["choices"].([]any)
	if !ok || len(choices) == 0 {
		return false
	}
	ch, ok := choices[0].(map[string]any)
	if !ok {
		return false
	}
	delta, ok := ch["delta"].(map[string]any)
	if !ok {
		return false
	}
	prev, _ := delta["content"].(string)
	delta["content"] = prev + content
	return true
}
