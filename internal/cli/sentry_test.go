package cli

import (
	"context"
	"errors"
	"log/slog"
	"testing"
)

var errHandlerFailed = errors.New("handler failed")

// recordingHandler appends its name as an attribute to observable record mutations.
type recordingHandler struct {
	name    string
	records *[]slog.Record
	err     error
	level   slog.Level
}

func (h recordingHandler) Enabled(_ context.Context, lvl slog.Level) bool { return lvl >= h.level }

func (h recordingHandler) Handle(_ context.Context, r slog.Record) error {
	r.AddAttrs(slog.String("handler", h.name))
	*h.records = append(*h.records, r)
	return h.err
}

func (h recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h recordingHandler) WithGroup(string) slog.Handler      { return h }

// crowdedRecord returns a record with enough attributes to trigger the overflow slice,
// where concurrent mutations without Clone cause memory corruption.
func crowdedRecord(t *testing.T) slog.Record {
	t.Helper()

	r := slog.Record{Level: slog.LevelInfo, Message: "hello"}
	for i := range 8 {
		r.AddAttrs(slog.Int("attr", i))
	}
	return r
}

// stampedBy extracts the handler stamp and checks if slog flagged unsafe sharing with "!BUG".
func stampedBy(t *testing.T, r slog.Record) (stamp string, shared bool) {
	t.Helper()

	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "handler":
			stamp = a.Value.String()
		case "!BUG":
			shared = true
		}
		return true
	})
	return stamp, shared
}

func TestReportPanicReraises(t *testing.T) {
	t.Parallel()

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		defer reportPanic()
		panic("boom")
	}()

	if recovered != "boom" {
		t.Errorf("recovered = %v, want %q", recovered, "boom")
	}
}

func TestReportPanicPassesThroughWithoutPanic(t *testing.T) {
	t.Parallel()

	func() {
		defer reportPanic()
	}()
}

func TestFanoutHandlerClonesRecords(t *testing.T) {
	t.Parallel()

	var first, second []slog.Record
	h := fanoutHandler{handlers: []slog.Handler{
		recordingHandler{name: "first", records: &first},
		recordingHandler{name: "second", records: &second},
	}}

	if err := h.Handle(t.Context(), crowdedRecord(t)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("records = (%d, %d), want (1, 1)", len(first), len(second))
	}
	for want, records := range map[string][]slog.Record{"first": first, "second": second} {
		got, shared := stampedBy(t, records[0])
		if shared {
			t.Errorf("%s handler was handed a record slog reports as an unsafely shared copy: it was not cloned", want)
		}
		if got != want {
			t.Errorf("%s handler's record is stamped %q, want %q", want, got, want)
		}
	}
}

func TestFanoutHandlerReachesEveryHandler(t *testing.T) {
	t.Parallel()

	var failing, working []slog.Record
	h := fanoutHandler{handlers: []slog.Handler{
		recordingHandler{name: "failing", records: &failing, err: errHandlerFailed},
		recordingHandler{name: "working", records: &working},
	}}

	err := h.Handle(t.Context(), slog.Record{Level: slog.LevelInfo, Message: "hello"})
	if !errors.Is(err, errHandlerFailed) {
		t.Errorf("Handle = %v, want %v", err, errHandlerFailed)
	}
	if len(working) != 1 {
		t.Errorf("handler behind the failing one saw %d records, want 1", len(working))
	}
}

func TestFanoutHandlerSkipsDisabledHandlers(t *testing.T) {
	t.Parallel()

	var debug, errorOnly []slog.Record
	h := fanoutHandler{handlers: []slog.Handler{
		recordingHandler{name: "debug", records: &debug, level: slog.LevelDebug},
		recordingHandler{name: "error", records: &errorOnly, level: slog.LevelError},
	}}

	if !h.Enabled(t.Context(), slog.LevelInfo) {
		t.Error("Enabled(Info) = false, want true: one handler accepts it")
	}
	if err := h.Handle(t.Context(), slog.Record{Level: slog.LevelInfo, Message: "hello"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(debug) != 1 {
		t.Errorf("debug handler saw %d records, want 1", len(debug))
	}
	if len(errorOnly) != 0 {
		t.Errorf("error-only handler saw %d records, want 0", len(errorOnly))
	}
}
