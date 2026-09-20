// Package eventstore is the audit-log contract: review/description events and
// every notification delivery attempt. History only, never a decision input
// (see internal/store); writes are best-effort so a database outage can't
// block a notification. Implementations live in internal/db.
package eventstore

import (
	"context"
	"time"

	"github.com/ConnorsApps/frigate-notifications/internal/frigate"
)

// Retention is how long records live before the backend reaps them.
const Retention = 365 * 24 * time.Hour

// Store is the audit sink. Save* are fire-and-forget (never block, never
// error); the read methods back cmd/events.
type Store interface {
	SaveReviewEvent(ctx context.Context, review frigate.ReviewPayload, lifecycle frigate.LifecycleType)
	SaveDescriptionUpdate(ctx context.Context, upd frigate.TrackedObjectUpdate)
	SaveNotification(ctx context.Context, rec NotificationRecord)

	// Reviews returns reviews since the cutoff, oldest first. Empty camera
	// matches all.
	Reviews(ctx context.Context, since time.Time, camera string) ([]ReviewEventRecord, error)
	// CountDescriptions counts description updates since the cutoff.
	CountDescriptions(ctx context.Context, since time.Time) (int64, error)
	// Notifications returns delivery attempts since the cutoff, oldest first.
	// Empty rule or recipient matches all.
	Notifications(ctx context.Context, since time.Time, rule, recipient string) ([]NotificationRecord, error)
}

// Metrics is the observability hook; must be cheap and non-blocking.
type Metrics interface {
	EventStoreWrite(op, result string)
}

// Kind discriminates event shapes.
const (
	KindReview            = "review"
	KindDescriptionUpdate = "description_update"
)

// ReviewEventRecord backs KindReview events.
type ReviewEventRecord struct {
	Kind          string    `bson:"kind" json:"kind"`
	ReceivedAt    time.Time `bson:"receivedAt" json:"receivedAt"`
	ReviewID      string    `bson:"reviewId" json:"reviewId"`
	EventID       string    `bson:"eventId,omitempty" json:"eventId,omitempty"`
	LifecycleType string    `bson:"lifecycleType" json:"lifecycleType"`
	Camera        string    `bson:"camera" json:"camera"`
	Severity      string    `bson:"severity" json:"severity"`
	StartTime     float64   `bson:"startTime" json:"startTime"`
	EndTime       *float64  `bson:"endTime,omitempty" json:"endTime,omitempty"`
	Objects       []string  `bson:"objects,omitempty" json:"objects,omitempty"`
	SubLabels     []string  `bson:"subLabels,omitempty" json:"subLabels,omitempty"`
	Zones         []string  `bson:"zones,omitempty" json:"zones,omitempty"`
	Detections    []string  `bson:"detections,omitempty" json:"detections,omitempty"`
	ThumbPath     string    `bson:"thumbPath,omitempty" json:"thumbPath,omitempty"`
}

// DescriptionUpdateRecord backs KindDescriptionUpdate events.
type DescriptionUpdateRecord struct {
	Kind        string    `bson:"kind" json:"kind"`
	ReceivedAt  time.Time `bson:"receivedAt" json:"receivedAt"`
	EventID     string    `bson:"eventId" json:"eventId"`
	Description string    `bson:"description" json:"description"`
}

// NotificationRecord backs one delivery attempt, including dry-run and failed.
type NotificationRecord struct {
	SentAt    time.Time `bson:"sentAt" json:"sentAt"`
	ReviewID  string    `bson:"reviewId" json:"reviewId"`
	EventID   string    `bson:"eventId,omitempty" json:"eventId,omitempty"`
	Rule      string    `bson:"rule" json:"rule"`
	Recipient string    `bson:"recipient" json:"recipient"`
	// Backend is the target type (hass, slack, ...); Target identifies which
	// of the recipient's targets on it, with no credentials.
	Backend    string `bson:"backend" json:"backend"`
	Target     string `bson:"target" json:"target"`
	Phase      string `bson:"phase" json:"phase"`
	Preset     string `bson:"preset" json:"preset"`
	Critical   bool   `bson:"critical" json:"critical"`
	DryRun     bool   `bson:"dryRun" json:"dryRun"`
	Success    bool   `bson:"success" json:"success"`
	Error      string `bson:"error,omitempty" json:"error,omitempty"`
	Title      string `bson:"title,omitempty" json:"title,omitempty"`
	Message    string `bson:"message,omitempty" json:"message,omitempty"`
	Tag        string `bson:"tag,omitempty" json:"tag,omitempty"`
	Image      string `bson:"image,omitempty" json:"image,omitempty"`
	Video      string `bson:"video,omitempty" json:"video,omitempty"`
	LiveEntity string `bson:"liveEntity,omitempty" json:"liveEntity,omitempty"`
	ClipURL    string `bson:"clipUrl,omitempty" json:"clipUrl,omitempty"`
}

// NewReviewEventRecord builds the record for a review lifecycle event.
func NewReviewEventRecord(review frigate.ReviewPayload, lifecycle frigate.LifecycleType) ReviewEventRecord {
	return ReviewEventRecord{
		Kind:          KindReview,
		ReceivedAt:    time.Now(),
		ReviewID:      review.ID,
		EventID:       review.PrimaryEventID(),
		LifecycleType: string(lifecycle),
		Camera:        review.Camera,
		Severity:      string(review.Severity),
		StartTime:     review.StartTime,
		EndTime:       review.EndTime,
		Objects:       review.Data.Objects,
		SubLabels:     review.Data.SubLabels,
		Zones:         review.Data.Zones,
		Detections:    review.Data.Detections,
		ThumbPath:     review.ThumbPath,
	}
}

// NewDescriptionUpdateRecord builds the record for a description update.
func NewDescriptionUpdateRecord(upd frigate.TrackedObjectUpdate) DescriptionUpdateRecord {
	return DescriptionUpdateRecord{
		Kind:        KindDescriptionUpdate,
		ReceivedAt:  time.Now(),
		EventID:     upd.ID,
		Description: upd.Description,
	}
}
