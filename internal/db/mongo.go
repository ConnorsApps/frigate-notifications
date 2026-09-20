package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ConnorsApps/frigate-notifications/internal/eventstore"
	"github.com/ConnorsApps/frigate-notifications/internal/frigate"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.opentelemetry.io/contrib/instrumentation/go.mongodb.org/mongo-driver/v2/mongo/otelmongo"
)

// mongoDBName is explicit: replica-set URIs carry no database path.
const mongoDBName = "frigate-notify"

type mongoDB struct {
	client        *mongo.Client
	events        *mongo.Collection // reviews + description updates
	notifications *mongo.Collection // one per delivery attempt
	kv            *mongo.Collection // decision state
	metrics       eventstore.Metrics
	logger        zerolog.Logger
}

// kvDoc is one state entry; N backs counters, Value everything else.
type kvDoc struct {
	ID        string    `bson:"_id"`
	Value     string    `bson:"value,omitempty"`
	N         int64     `bson:"n,omitempty"`
	ExpiresAt time.Time `bson:"expiresAt"`
}

func openMongo(ctx context.Context, uri string, m eventstore.Metrics, o openOptions) (*mongoDB, error) {
	client, err := mongo.Connect(options.Client().ApplyURI(uri).SetMonitor(otelmongo.NewMonitor()))
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}

	d := client.Database(mongoDBName)
	c := &mongoDB{
		client:        client,
		events:        d.Collection("mqtt_events"),
		notifications: d.Collection("notifications"),
		kv:            d.Collection("kv"),
		metrics:       m,
		logger:        log.With().Str("logger", "db").Str("kind", string(KindMongo)).Logger(),
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx, nil); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, fmt.Errorf("ping: %w", err)
	}

	// Best-effort: a conflicting leftover index shouldn't disable persistence.
	if !o.readOnly {
		if err := c.ensureIndexes(ctx); err != nil {
			c.logger.Warn().Err(err).Msg("failed to ensure indexes; continuing without them")
		}
	}
	c.logger.Info().Str("db", mongoDBName).Msg("connected to mongo")
	return c, nil
}

func (c *mongoDB) Close(ctx context.Context) error {
	return c.client.Disconnect(ctx)
}

func (c *mongoDB) ensureIndexes(ctx context.Context) error {
	ttlSeconds := int32(eventstore.Retention.Seconds())

	if _, err := c.events.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "receivedAt", Value: 1}},
			Options: options.Index().SetExpireAfterSeconds(ttlSeconds),
		},
		{Keys: bson.D{{Key: "reviewId", Value: 1}, {Key: "eventId", Value: 1}}},
	}); err != nil {
		return fmt.Errorf("mqtt_events indexes: %w", err)
	}

	if _, err := c.notifications.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "sentAt", Value: 1}},
			Options: options.Index().SetExpireAfterSeconds(ttlSeconds),
		},
		{Keys: bson.D{{Key: "reviewId", Value: 1}}},
	}); err != nil {
		return fmt.Errorf("notifications indexes: %w", err)
	}

	// The reaper lags ~1min, so kv queries also check expiresAt themselves.
	if _, err := c.kv.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "expiresAt", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(0),
	}); err != nil {
		return fmt.Errorf("kv indexes: %w", err)
	}
	return nil
}

// --- eventstore.Store ---

func (c *mongoDB) SaveReviewEvent(_ context.Context, review frigate.ReviewPayload, lifecycle frigate.LifecycleType) {
	c.insert("saveReviewEvent", c.events, eventstore.NewReviewEventRecord(review, lifecycle))
}

func (c *mongoDB) SaveDescriptionUpdate(_ context.Context, upd frigate.TrackedObjectUpdate) {
	c.insert("saveDescriptionUpdate", c.events, eventstore.NewDescriptionUpdateRecord(upd))
}

func (c *mongoDB) SaveNotification(_ context.Context, rec eventstore.NotificationRecord) {
	c.insert("saveNotification", c.notifications, rec)
}

func (c *mongoDB) insert(op string, coll *mongo.Collection, doc any) {
	writeAsync(c.logger, c.metrics, op, func(ctx context.Context) error {
		_, err := coll.InsertOne(ctx, doc)
		return err
	})
}

func (c *mongoDB) Reviews(ctx context.Context, since time.Time, camera string) ([]eventstore.ReviewEventRecord, error) {
	filter := bson.M{"kind": eventstore.KindReview, "receivedAt": bson.M{"$gte": since}}
	if camera != "" {
		filter["camera"] = camera
	}
	return findAll[eventstore.ReviewEventRecord](ctx, c.events, filter, "receivedAt")
}

func (c *mongoDB) CountDescriptions(ctx context.Context, since time.Time) (int64, error) {
	return c.events.CountDocuments(ctx,
		bson.M{"kind": eventstore.KindDescriptionUpdate, "receivedAt": bson.M{"$gte": since}})
}

func (c *mongoDB) Notifications(ctx context.Context, since time.Time, rule, recipient string) ([]eventstore.NotificationRecord, error) {
	filter := bson.M{"sentAt": bson.M{"$gte": since}}
	if rule != "" {
		filter["rule"] = rule
	}
	if recipient != "" {
		filter["recipient"] = recipient
	}
	return findAll[eventstore.NotificationRecord](ctx, c.notifications, filter, "sentAt")
}

// findAll returns the documents matching filter, oldest first by sortKey.
func findAll[T any](ctx context.Context, coll *mongo.Collection, filter bson.M, sortKey string) ([]T, error) {
	cur, err := coll.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: sortKey, Value: 1}}))
	if err != nil {
		return nil, err
	}
	var out []T
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// --- store.Backend ---

// SetNX claims key if absent or expired. The filter only matches expired
// entries, so against a live one the upsert's insert collides on _id.
func (c *mongoDB) SetNX(ctx context.Context, key, val string, ttl time.Duration) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, kvTimeout)
	defer cancel()
	now := time.Now()
	_, err := c.kv.UpdateOne(ctx,
		bson.M{"_id": key, "expiresAt": bson.M{"$lte": now}},
		bson.M{"$set": bson.M{"value": val, "expiresAt": now.Add(ttl)}},
		options.UpdateOne().SetUpsert(true))
	if mongo.IsDuplicateKeyError(err) {
		return false, nil
	}
	return err == nil, err
}

func (c *mongoDB) Get(ctx context.Context, key string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, kvTimeout)
	defer cancel()
	var doc kvDoc
	err := c.kv.FindOne(ctx, bson.M{"_id": key, "expiresAt": bson.M{"$gt": time.Now()}}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return doc.Value, true, nil
}

func (c *mongoDB) Set(ctx context.Context, key, val string, ttl time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, kvTimeout)
	defer cancel()
	_, err := c.kv.ReplaceOne(ctx, bson.M{"_id": key},
		kvDoc{ID: key, Value: val, ExpiresAt: time.Now().Add(ttl)},
		options.Replace().SetUpsert(true))
	return err
}

func (c *mongoDB) Del(ctx context.Context, key string) error {
	ctx, cancel := context.WithTimeout(ctx, kvTimeout)
	defer cancel()
	_, err := c.kv.DeleteOne(ctx, bson.M{"_id": key})
	return err
}

// Incr is one atomic pipeline update: expired or new restarts at 1, else +1,
// and the ttl is refreshed. Racing first calls can collide on the upsert's
// insert, so a duplicate-key error is retried once.
func (c *mongoDB) Incr(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, kvTimeout)
	defer cancel()

	for attempt := 0; ; attempt++ {
		now := time.Now()
		expired := bson.D{{Key: "$lte", Value: bson.A{
			bson.D{{Key: "$ifNull", Value: bson.A{"$expiresAt", time.Time{}}}}, now,
		}}}
		update := mongo.Pipeline{{{Key: "$set", Value: bson.D{
			{Key: "n", Value: bson.D{{Key: "$cond", Value: bson.A{
				expired, int64(1), bson.D{{Key: "$add", Value: bson.A{"$n", int64(1)}}},
			}}}},
			{Key: "expiresAt", Value: now.Add(ttl)},
		}}}}

		var doc kvDoc
		err := c.kv.FindOneAndUpdate(ctx, bson.M{"_id": key}, update,
			options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After),
		).Decode(&doc)
		if err == nil {
			return doc.N, nil
		}
		if mongo.IsDuplicateKeyError(err) && attempt == 0 {
			continue
		}
		return 0, err
	}
}
