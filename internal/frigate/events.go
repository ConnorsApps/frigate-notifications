// Package frigate holds Go types for Frigate's MQTT payloads and a thin
// subscriber. ReviewEvent is verified against real captures (see testdata);
// the "genai" message and ReviewMetadata are modelled from Frigate 0.17+
// source, so testdata/review-genai.json is synthetic.
//
// Face recognition and GenAI are enabled on this instance. ReviewData.SubLabels
// holds Frigate's committed name, the only field notification text should
// read. TrackedObjectUpdate's "face" variant is unverified per-attempt data.
package frigate

import "encoding/json"

// LifecycleType is the "new"/"update"/"end" state on ReviewEvent.
type LifecycleType string

const (
	LifecycleNew    LifecycleType = "new"
	LifecycleUpdate LifecycleType = "update"
	LifecycleEnd    LifecycleType = "end"
	// LifecycleGenAI arrives after "end" with data.metadata filled in. Not a
	// rule phase.
	LifecycleGenAI LifecycleType = "genai"
)

// Severity is ReviewPayload.Severity.
type Severity string

const (
	SeverityAlert     Severity = "alert"
	SeverityDetection Severity = "detection"
)

// ReviewEvent is the payload on "<topic_prefix>/reviews".
type ReviewEvent struct {
	Type   LifecycleType `json:"type"`
	Before ReviewPayload `json:"before"`
	After  ReviewPayload `json:"after"`
}

type ReviewPayload struct {
	ID        string     `json:"id"`
	Camera    string     `json:"camera"`
	StartTime float64    `json:"start_time"`
	EndTime   *float64   `json:"end_time"`
	Severity  Severity   `json:"severity"`
	ThumbPath string     `json:"thumb_path"`
	Data      ReviewData `json:"data"`
}

// PrimaryEventID returns the Frigate event id backing a review, or "". Media
// and GenAI descriptions key off the event id, not the review id.
func (p ReviewPayload) PrimaryEventID() string {
	if len(p.Data.Detections) == 0 {
		return ""
	}
	return p.Data.Detections[0]
}

type ReviewData struct {
	// Detections holds the underlying Frigate event IDs.
	Detections []string `json:"detections"`
	Objects    []string `json:"objects"`
	SubLabels  []string `json:"sub_labels"`
	Zones      []string `json:"zones"`
	Audio      []string `json:"audio"`
	// Null except on "genai". Raw, so an odd shape can't fail the message.
	Metadata json.RawMessage `json:"metadata"`
}

// ReviewMetadata is Frigate's GenAI review summary; validated softly, so any
// field may be missing or out of range.
type ReviewMetadata struct {
	Title        string `json:"title"`
	ShortSummary string `json:"shortSummary"` // written for notifications
	// PotentialThreatLevel: 0 normal, 1 suspicious, 2 critical.
	PotentialThreatLevel int `json:"potential_threat_level"`
}

// GenAI returns the summary, or nil if absent or unusable.
func (d ReviewData) GenAI() *ReviewMetadata {
	if len(d.Metadata) == 0 || string(d.Metadata) == "null" {
		return nil
	}
	var m ReviewMetadata
	if err := json.Unmarshal(d.Metadata, &m); err != nil {
		return nil
	}
	if m.Title == "" && m.ShortSummary == "" {
		return nil
	}
	return &m
}

// TrackedObjectUpdateType: "description" (GenAI) or "face" (a per-attempt
// face-recognition result).
type TrackedObjectUpdateType string

const (
	TrackedObjectUpdateDescription TrackedObjectUpdateType = "description"
	TrackedObjectUpdateFace        TrackedObjectUpdateType = "face"
)

// TrackedObjectUpdate is the payload on "<topic_prefix>/tracked_object_update".
// Fields are populated per Type. A "face" update is one recognition attempt,
// not the review's final sub_label (Frigate weighs several before committing
// one) — never build notification text from it.
type TrackedObjectUpdate struct {
	Type        TrackedObjectUpdateType `json:"type"`
	ID          string                  `json:"id"`
	Description string                  `json:"description"`

	// Name/Score: "face" only. Name is nil on a miss; Score is a running
	// weighted average, not one frame's confidence.
	Name  *string `json:"name"`
	Score float64 `json:"score"`
}
