package clips

import (
	"strings"
	"testing"
	"time"

	"backtrack/internal/model"
)

func TestVirtualClipOffsetsSkipAndFillGaps(t *testing.T) {
	origin := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	segments := []model.Segment{
		{StartTime: origin, EndTime: origin.Add(2 * time.Second)},
		{StartTime: origin.Add(5 * time.Second), EndTime: origin.Add(7 * time.Second)},
	}
	req := model.ClipRequest{StartTime: origin.Add(time.Second), EndTime: origin.Add(6 * time.Second)}

	start, end := virtualClipOffsets(segments, req, false)
	if start != 1 || end != 3 {
		t.Fatalf("skip offsets = %.3f–%.3f, want 1.000–3.000", start, end)
	}
	start, end = virtualClipOffsets(segments, req, true)
	if start != 1 || end != 6 {
		t.Fatalf("fill offsets = %.3f–%.3f, want 1.000–6.000", start, end)
	}
}

func TestValidateExportDurationRejectsTruncatedOutput(t *testing.T) {
	if err := validateExportDuration(4, 15.3, "accurate", 2*time.Second); err == nil {
		t.Fatal("accurate export with a truncated duration was accepted")
	}
	if err := validateExportDuration(4, 8, "fast", 2*time.Second); err == nil || !strings.Contains(err.Error(), "shorter than expected") {
		t.Fatalf("truncated fast export error = %v, want duration rejection", err)
	}
}

func TestValidateExportDurationAllowsExpectedFastBoundaryDrift(t *testing.T) {
	if err := validateExportDuration(4.8, 6, "fast", 2*time.Second); err != nil {
		t.Fatalf("fast export with keyframe boundary drift was rejected: %v", err)
	}
	if err := validateExportDuration(5.6, 5, "accurate", 2*time.Second); err == nil {
		t.Fatal("accurate export outside the frame/timing tolerance was accepted")
	}
}

func TestVirtualClipOffsetsFillLeadingAndTrailingGaps(t *testing.T) {
	origin := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	segments := []model.Segment{{StartTime: origin, EndTime: origin.Add(2 * time.Second)}}
	req := model.ClipRequest{StartTime: origin.Add(-time.Second), EndTime: origin.Add(3 * time.Second)}
	start, end := virtualClipOffsets(segments, req, true)
	if start != 0 || end != 4 {
		t.Fatalf("leading/trailing fill offsets = %.3f–%.3f, want 0.000–4.000", start, end)
	}
}
