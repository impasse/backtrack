package model

import "time"

type Stream struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"createdAt"`
	PublishKey string    `json:"publishKey,omitempty"`
}

type Session struct {
	ID           string     `json:"id"`
	StreamID     string     `json:"streamId"`
	StartedAt    time.Time  `json:"startedAt"`
	EndedAt      *time.Time `json:"endedAt,omitempty"`
	Status       string     `json:"status"`
	SegmentCount int        `json:"segmentCount"`
}

type Epoch struct {
	ID              string     `json:"id"`
	SessionID       string     `json:"sessionId"`
	StartedAt       time.Time  `json:"startedAt"`
	EndedAt         *time.Time `json:"endedAt,omitempty"`
	InitPath        string     `json:"-"`
	CodecConfigHash string     `json:"codecConfigHash"`
	VideoTimescale  uint32     `json:"videoTimescale"`
	AudioTimescale  uint32     `json:"audioTimescale"`
}

type Segment struct {
	ID           string    `json:"id"`
	StreamID     string    `json:"streamId"`
	SessionID    string    `json:"sessionId"`
	EpochID      string    `json:"epochId"`
	Sequence     uint64    `json:"sequence"`
	StartTime    time.Time `json:"startTime"`
	EndTime      time.Time `json:"endTime"`
	FirstPTS     int64     `json:"firstPts"`
	LastPTS      int64     `json:"lastPts"`
	FirstDTS     int64     `json:"firstDts"`
	LastDTS      int64     `json:"lastDts"`
	TimeScale    uint32    `json:"timeScale"`
	Path         string    `json:"-"`
	SizeBytes    int64     `json:"sizeBytes"`
	RandomAccess bool      `json:"randomAccess"`
	Checksum     string    `json:"checksum"`
	State        string    `json:"state"`
	Pinned       bool      `json:"pinned"`
}

type Gap struct {
	StreamID  string    `json:"streamId"`
	SessionID string    `json:"sessionId"`
	StartTime time.Time `json:"startTime"`
	EndTime   time.Time `json:"endTime"`
	Reason    string    `json:"reason"`
}

type TimeRange struct {
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	Available  bool      `json:"available"`
	Reason     string    `json:"reason,omitempty"`
	SessionID  string    `json:"sessionId,omitempty"`
	SegmentIDs []string  `json:"segmentIds,omitempty"`
	Pinned     bool      `json:"pinned,omitempty"`
	Merged     bool      `json:"merged,omitempty"`
}

type TimelineBucket struct {
	Start        time.Time `json:"start"`
	End          time.Time `json:"end"`
	RecordedMS   int64     `json:"recordedMs"`
	SegmentCount int64     `json:"segmentCount"`
}

type HistoryRangeSelection struct {
	SessionID string    `json:"sessionId"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
}

type ClipRequest struct {
	Name          string    `json:"name,omitempty"`
	SourceClipID  string    `json:"sourceClipId,omitempty"`
	StreamID      string    `json:"streamId"`
	StartTime     time.Time `json:"startTime"`
	EndTime       time.Time `json:"endTime"`
	Mode          string    `json:"mode"`
	QualityPreset string    `json:"qualityPreset,omitempty"`
	Resolution    string    `json:"resolution,omitempty"`
	GapPolicy     string    `json:"gapPolicy"`
	SaveToLibrary bool      `json:"saveToLibrary"`
}

type ClipJob struct {
	ID         string      `json:"id"`
	Request    ClipRequest `json:"request"`
	Status     string      `json:"status"`
	Progress   float64     `json:"progress"`
	Error      string      `json:"error,omitempty"`
	OutputPath string      `json:"-"`
	CreatedAt  time.Time   `json:"createdAt"`
	UpdatedAt  time.Time   `json:"updatedAt"`
	Pinned     bool        `json:"pinned"`
}
