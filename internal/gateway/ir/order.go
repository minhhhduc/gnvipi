package ir

import "fmt"

// ValidateEventOrder checks that a slice of StreamEvent obeys the ordering
// contract documented on the package:
//
//   - exactly one message_start, and it comes first;
//   - each block index is opened by block_start at most once, and its
//     block_delta and block_stop events come after that block_start and
//     before the terminal event; every opened block must be closed by
//     block_stop before message_stop;
//   - at most one usage_update, and it comes after message_start;
//   - exactly one terminal event (message_stop or error) and nothing after
//     it; message_stop must carry a non-empty stop reason.
//
// Adapters are expected to produce compliant sequences by construction;
// this helper exists so tests can keep them honest. The empty slice is
// invalid.
func ValidateEventOrder(events []StreamEvent) error {
	if len(events) == 0 {
		return fmt.Errorf("ir: empty stream event sequence")
	}
	started := false
	openBlocks := make(map[int]bool)
	closedBlocks := make(map[int]bool)
	usageSent := false
	terminated := false

	for i, ev := range events {
		if terminated {
			return fmt.Errorf("ir: event %d (%s) after terminal event at index %d", i, ev.Type, i-1)
		}
		switch ev.Type {
		case EventMessageStart:
			if started {
				return fmt.Errorf("ir: duplicate message_start at index %d", i)
			}
			if i != 0 {
				return fmt.Errorf("ir: message_start at index %d is not the first event", i)
			}
			started = true
		case EventBlockStart:
			if !started {
				return fmt.Errorf("ir: block_start at index %d before message_start", i)
			}
			if closedBlocks[ev.Index] {
				return fmt.Errorf("ir: block_start at index %d reopens a closed block", i)
			}
			if openBlocks[ev.Index] {
				return fmt.Errorf("ir: duplicate block_start for index %d at event %d", ev.Index, i)
			}
			openBlocks[ev.Index] = true
		case EventBlockDelta:
			if !started {
				return fmt.Errorf("ir: block_delta at index %d before message_start", i)
			}
			if !openBlocks[ev.Index] {
				return fmt.Errorf("ir: block_delta at event %d for unopened block index %d", i, ev.Index)
			}
		case EventBlockStop:
			if !started {
				return fmt.Errorf("ir: block_stop at index %d before message_start", i)
			}
			if !openBlocks[ev.Index] {
				return fmt.Errorf("ir: block_stop at event %d for unopened block index %d", i, ev.Index)
			}
			openBlocks[ev.Index] = false
			closedBlocks[ev.Index] = true
		case EventUsageUpdate:
			if !started {
				return fmt.Errorf("ir: usage_update at index %d before message_start", i)
			}
			if usageSent {
				return fmt.Errorf("ir: duplicate usage_update at index %d", i)
			}
			usageSent = true
		case EventMessageStop:
			if !started {
				return fmt.Errorf("ir: message_stop at index %d before message_start", i)
			}
			if ev.StopReason == "" {
				return fmt.Errorf("ir: message_stop at index %d has no stop reason", i)
			}
			for idx := range openBlocks {
				if openBlocks[idx] {
					return fmt.Errorf("ir: message_stop at index %d closes the stream with block %d still open", i, idx)
				}
			}
			terminated = true
		case EventError:
			if ev.Error == nil {
				return fmt.Errorf("ir: error event at index %d has no error payload", i)
			}
			terminated = true
		default:
			return fmt.Errorf("ir: unknown event type %q at index %d", ev.Type, i)
		}
	}
	if !terminated {
		return fmt.Errorf("ir: stream event sequence has no terminal event")
	}
	return nil
}
