package lambda

import (
	"strconv"
	"strings"
)

// report is what the adapter could parse out of a Lambda REPORT line. Every
// field is a pointer because "Lambda did not tell us" and "Lambda told us
// zero" are different facts, and the evidence mapping treats them
// differently: the first produces {measured:false}, the second a measured
// zero.
type report struct {
	RequestID    string
	DurationMs   *float64
	BilledMs     *float64
	MemorySizeMB *float64
	MaxMemoryMB  *float64
	InitMs       *float64
}

// found reports whether a REPORT line was present at all.
func (r report) found() bool { return r.DurationMs != nil || r.BilledMs != nil || r.MaxMemoryMB != nil }

// parseReport extracts the platform's own measurements from an execution log
// tail. Lambda emits one REPORT line per invocation, tab-separated:
//
//	REPORT RequestId: 8f5…\tDuration: 1234.56 ms\tBilled Duration: 1235 ms\t
//	Memory Size: 2048 MB\tMax Memory Used: 128 MB\tInit Duration: 300.00 ms
//
// The parse is deliberately tolerant: an unrecognised field is skipped, a
// missing field stays nil, and a log tail with no REPORT line at all (the
// 4 KB window can be filled by the function's own output) yields a zero
// report whose found() is false. Every one of those cases ends in the same
// place — the resource_usage observation says {measured:false,
// complete:false} — rather than in a guessed number.
//
// The line is attributed to the invocation whose response carried it. The
// adapter does not compare REPORT's RequestId against the API response's
// request id: whether those two identifiers are always the same value is not
// something this code can verify, and an equality check it cannot justify
// would be a fabricated guarantee.
func parseReport(logTail string) report {
	var out report
	for _, line := range strings.Split(logTail, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "REPORT") {
			continue
		}
		out = parseReportLine(strings.TrimPrefix(line, "REPORT"))
		// The last REPORT in the window is the one for this invocation.
	}
	return out
}

func parseReportLine(line string) report {
	var out report
	for _, field := range strings.Split(line, "\t") {
		key, value, ok := strings.Cut(strings.TrimSpace(field), ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		if key == "RequestId" {
			out.RequestID = value
			continue
		}

		number, ok := parseLeadingNumber(value)
		if !ok {
			continue
		}
		switch key {
		case "Duration":
			out.DurationMs = &number
		case "Billed Duration":
			out.BilledMs = &number
		case "Memory Size":
			out.MemorySizeMB = &number
		case "Max Memory Used":
			out.MaxMemoryMB = &number
		case "Init Duration":
			out.InitMs = &number
		}
	}
	return out
}

// parseLeadingNumber reads the numeric part of a "1234.56 ms" or "128 MB"
// value, ignoring the unit. Lambda reports durations in milliseconds and
// memory in megabytes throughout, so the unit carries no information the
// field name does not already carry.
func parseLeadingNumber(value string) (float64, bool) {
	number, _, _ := strings.Cut(value, " ")
	parsed, err := strconv.ParseFloat(number, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}
