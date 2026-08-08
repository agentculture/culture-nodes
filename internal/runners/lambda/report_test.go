package lambda

import "testing"

// The REPORT line is the only place Lambda states billed duration and peak
// memory, so every way it can be absent or malformed has to end in "not
// measured" rather than in a zero that reads like a reading.

func TestParseReportReadsEveryField(t *testing.T) {
	log := "START RequestId: 8f5a Version: 7\n" +
		"hello from the function\n" +
		"END RequestId: 8f5a\n" +
		"REPORT RequestId: 8f5a\tDuration: 1234.56 ms\tBilled Duration: 1235 ms\t" +
		"Memory Size: 2048 MB\tMax Memory Used: 128 MB\tInit Duration: 312.44 ms\t\n"

	rep := parseReport(log)
	if !rep.found() {
		t.Fatal("found() = false for a log containing a REPORT line")
	}
	if rep.RequestID != "8f5a" {
		t.Errorf("RequestID = %q", rep.RequestID)
	}
	for _, tc := range []struct {
		name string
		got  *float64
		want float64
	}{
		{"Duration", rep.DurationMs, 1234.56},
		{"Billed Duration", rep.BilledMs, 1235},
		{"Memory Size", rep.MemorySizeMB, 2048},
		{"Max Memory Used", rep.MaxMemoryMB, 128},
		{"Init Duration", rep.InitMs, 312.44},
	} {
		if tc.got == nil {
			t.Errorf("%s not parsed", tc.name)
			continue
		}
		if *tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, *tc.got, tc.want)
		}
	}
}

func TestParseReportOnAbsentOrUnusableInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		log  string
	}{
		{"empty", ""},
		{"no REPORT line", "START RequestId: 8f5a Version: 7\nhello\nEND RequestId: 8f5a\n"},
		{"truncated mid-line", "REPORT RequestId: 8f5a\tDurat"},
		{"non-numeric values", "REPORT RequestId: 8f5a\tDuration: n/a ms\tBilled Duration: unknown ms\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep := parseReport(tc.log)
			if rep.found() {
				t.Errorf("found() = true for %q; an unreadable REPORT is not a measurement", tc.log)
			}
			if rep.DurationMs != nil || rep.BilledMs != nil || rep.MaxMemoryMB != nil {
				t.Errorf("values were invented from %q: %+v", tc.log, rep)
			}
		})
	}
}

// TestParseReportPrefersTheLastReport: a warm container's 4 KB window can
// still hold the previous invocation's REPORT. The last one is this
// invocation's.
func TestParseReportPrefersTheLastReport(t *testing.T) {
	log := "REPORT RequestId: older\tDuration: 10.00 ms\tBilled Duration: 10 ms\n" +
		"START RequestId: newer Version: 7\n" +
		"REPORT RequestId: newer\tDuration: 20.00 ms\tBilled Duration: 20 ms\n"

	rep := parseReport(log)
	if rep.RequestID != "newer" {
		t.Errorf("RequestID = %q, want the last REPORT's", rep.RequestID)
	}
	if rep.DurationMs == nil || *rep.DurationMs != 20 {
		t.Errorf("Duration = %v, want 20", rep.DurationMs)
	}
}

func TestParseLeadingNumberIgnoresUnits(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want float64
		ok   bool
	}{
		{"1234.56 ms", 1234.56, true},
		{"128 MB", 128, true},
		{"0 ms", 0, true},
		{"", 0, false},
		{"ms", 0, false},
	} {
		got, ok := parseLeadingNumber(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("parseLeadingNumber(%q) = %v, %t; want %v, %t", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
