package lithe

import (
	"slices"
	"strconv"
	"time"
)

// operations are the fixed operations a diagnostic may name besides the
// backend's registered functions.
var operations = []string{
	"initialize",
	"shutdown",
	"notification",
	"function-call",
	"result-encoding",
	"lithe/build/model",
	"lithe/build/dependencies",
	"lithe/build/execute",
	"lithe/build/test",
	"lithe/build/discoverTests",
	"lithe/build/prepareRun",
	"lithe/build/prepareDebug",
	"lithe/build/runEntrypoints",
	"lithe/vcs/status",
	"lithe/vcs/baseContent",
	"lithe/vcs/history",
	"lithe/vcs/fileHistory",
	"lithe/vcs/annotate",
	"lithe/vcs/checkin",
	"lithe/vcs/rollback",
	"lithe/vcs/update",
	"lithe/vcs/fileOperation",
	"lithe/vcs/resolve",
	"lithe/vcs/cleanup",
	methodKeymapImport,
}

// maxEarlyDiagnostics is how many facts recorded during initialize wait for
// its answer.
const maxEarlyDiagnostics = 8

// token reports whether value is 1-maximum ASCII letters, digits, ".", "_",
// ":", "/", "-" or "$".
func token(value string, maximum int) bool {
	if value == "" || len(value) > maximum {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if !isASCIIAlnum(c) && c != '.' && c != '_' && c != ':' && c != '/' && c != '-' && c != '$' {
			return false
		}
	}
	return true
}

// requestOperationID is the operation ID diagnostics carry: the call's
// operationID when it is a token, otherwise its JSON-RPC ID; "" for none.
func requestOperationID(operationID, requestID any) string {
	if id, ok := operationID.(string); ok && token(id, 128) {
		return id
	}
	var fallback string
	switch id := requestID.(type) {
	case string:
		fallback = id
	default:
		number, ok := integer(id)
		if !ok || number > maxSafeInteger || number < -maxSafeInteger {
			return ""
		}
		fallback = strconv.FormatInt(number, 10)
	}
	if !token(fallback, 128) {
		return ""
	}
	return fallback
}

// Diagnostics records typed facts in the host's litheLogging catalog over
// the protocol stream, without a queue or file I/O.
type Diagnostics struct {
	shared      *shared
	operationID string
}

// Degraded records a handled degradation. code is a controlled category,
// never an error message; pass at most one count.
func (d *Diagnostics) Degraded(operation, code string, count ...uint64) error {
	return d.record("degraded", operation, &code, first(count), nil)
}

// Recovered records a real recovery, with the same controlled operation
// and category as Degraded.
func (d *Diagnostics) Recovered(operation, code string, count ...uint64) error {
	return d.record("recovered", operation, &code, first(count), nil)
}

func first(values []uint64) *uint64 {
	if len(values) == 0 {
		return nil
	}
	return &values[0]
}

// terminal records how an operation ended; err is nil for success.
func (d *Diagnostics) terminal(operation string, err error, started time.Time) {
	kind := "operationFinished"
	var code *string
	if err != nil {
		kind = "operationFailed"
		// A plugin error logs its own code; any other failure only its kind.
		category := failureKind(err)
		if errorKind, plugin := classify(err); errorKind == kindPlugin {
			category = plugin.code
		}
		code = &category
	}
	duration := elapsed(started)
	_ = d.record(kind, operation, code, nil, &duration)
}

func elapsed(started time.Time) uint64 {
	milliseconds := time.Since(started).Milliseconds()
	if milliseconds < 0 {
		return 0
	}
	return min(uint64(milliseconds), maxSafeInteger)
}

func (d *Diagnostics) record(kind, operation string, code *string, count, duration *uint64) error {
	s := d.shared
	s.mu.Lock()
	_, registered := s.state.functions[operation]
	enabled := s.state.logging
	s.mu.Unlock()
	if !slices.Contains(operations, operation) && !registered {
		return newSdkError("sdk.logging.invalidOperation", "Diagnostics need a registered operation.")
	}
	if code != nil && !token(*code, 64) {
		return newSdkError("sdk.logging.invalidCode", "Diagnostics need a controlled code.")
	}
	if count != nil && *count > maxSafeInteger {
		return newSdkError("sdk.logging.invalidCount", "Diagnostics need a safe count.")
	}
	if !enabled {
		return nil
	}
	named := operation
	if len(named) > 64 {
		named = "function-call"
	}
	params := map[string]any{"kind": kind, "operation": named}
	if code != nil {
		params["code"] = *code
	}
	if count != nil {
		params["count"] = *count
	}
	if duration != nil {
		params["durationMs"] = *duration
	}
	if token(d.operationID, 128) {
		params["operationId"] = d.operationID
	}
	s.mu.Lock()
	if !s.state.loggingReady {
		if len(s.state.earlyDiagnostics) < maxEarlyDiagnostics {
			s.state.earlyDiagnostics = append(s.state.earlyDiagnostics, params)
		}
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	s.write(map[string]any{"jsonrpc": "2.0", "method": methodLog, "params": params})
	return nil
}
