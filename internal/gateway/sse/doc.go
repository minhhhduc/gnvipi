// Package sse provides a spec-compliant Server-Sent Events reader and
// writer for the gateway's streaming paths.
//
// Upstream SSE arrives over many providers' /v1/... endpoints and client
// responses are served as SSE to all three inbound protocols, so both
// directions need one shared implementation instead of the three ad-hoc
// ones the codebase currently carries (bufio.Scanner in the Anthropic
// relay, raw chunk copies in passthrough, the NVIDIA coalescer).
//
// The reader implements the HTML spec event-stream grammar: lines
// terminated by LF, CRLF or CR; "event:", "data:", "id:" and "retry:"
// fields with optional whitespace after the colon; ":" comment lines; and
// event dispatch on an empty line. Multi-line "data:" fields are joined
// with "\n" as the spec requires. There is no line length limit — the
// reader accumulates byte by byte behind a bufio.Reader, so events far
// larger than bufio.Scanner's 64 KiB cap parse correctly.
//
// The writer emits events with LF endings, writes multi-line data as
// successive "data:" lines, keeps comments available for keepalives, and
// flushes after every event through http.Flusher so streaming clients
// receive events as they are produced.
package sse
