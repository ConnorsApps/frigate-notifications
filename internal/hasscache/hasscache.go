// Package hasscache reads Home Assistant entity states for rule conditions,
// behind short caches so a burst of reviews doesn't fan out into a burst of
// HTTP calls.
package hasscache

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/ConnorsApps/frigate-notifications/internal/config"
)

// entityTTL is short: presence and alarm state change on human timescales,
// but a stale answer here decides whether a push is suppressed.
const entityTTL = 10 * time.Second

// solarTTL is long: sunset moves about a minute a day.
const solarTTL = 15 * time.Minute

// lookupTimeout bounds one Home Assistant read. Lookups run on the MQTT
// dispatch goroutine, so a hung Home Assistant must not stall every alert.
//
// failTTL is how long a failed read is remembered: while Home Assistant is down
// each evaluation would otherwise spend a full timeout, and those add up on
// that same goroutine. Both are vars so tests can shorten them.
var (
	lookupTimeout = 3 * time.Second
	failTTL       = 5 * time.Second
)

// StateReader is the subset of *hass.Client this package needs.
type StateReader interface {
	EntityState(ctx context.Context, entityID string) ([]byte, error)
}

type entry struct {
	state   string
	err     error // the failed read, remembered for failTTL
	fetched time.Time
}

type Cache struct {
	client StateReader
	loc    *time.Location

	mu       sync.Mutex
	entities map[string]entry

	solarMu    sync.Mutex
	solar      config.SolarTimes
	solarAt    time.Time // zero until the first successful fetch
	solarErr   error     // the latest failed fetch
	solarErrAt time.Time // zero while the latest fetch succeeded
}

func New(client StateReader, loc *time.Location) *Cache {
	return &Cache{
		client:   client,
		loc:      loc,
		entities: make(map[string]entry),
	}
}

// State returns an entity's state string.
func (c *Cache) State(ctx context.Context, entityID string) (string, error) {
	c.mu.Lock()
	e, ok := c.entities[entityID]
	c.mu.Unlock()
	if ok {
		ttl := entityTTL
		if e.err != nil {
			ttl = failTTL
		}
		if time.Since(e.fetched) < ttl {
			return e.state, e.err
		}
	}

	state, err := c.fetchState(ctx, entityID)
	// A canceled caller says nothing about Home Assistant.
	if err == nil || ctx.Err() == nil {
		c.mu.Lock()
		c.entities[entityID] = entry{state: state, err: err, fetched: time.Now()}
		c.mu.Unlock()
	}
	return state, err
}

func (c *Cache) fetchState(ctx context.Context, entityID string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()

	body, err := c.client.EntityState(ctx, entityID)
	if err != nil {
		return "", err
	}
	var payload struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("hasscache: parse %s: %w", entityID, err)
	}
	return payload.State, nil
}

// Solar returns today's solar times as durations since local midnight. A
// failed refresh keeps serving the last known good value: solar times are
// nearly constant, so a stale answer beats no answer.
func (c *Cache) Solar(ctx context.Context) (config.SolarTimes, error) {
	c.solarMu.Lock()
	defer c.solarMu.Unlock()

	haveLast := !c.solarAt.IsZero()
	if haveLast && time.Since(c.solarAt) < solarTTL {
		return c.solar, nil
	}
	// Don't retry a failing fetch on every call: the lock is held meanwhile.
	if c.solarErr != nil && time.Since(c.solarErrAt) < failTTL {
		if haveLast {
			return c.solar, nil
		}
		return config.SolarTimes{}, c.solarErr
	}

	fetchCtx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	s, err := c.fetchSolar(fetchCtx)
	if err != nil {
		if ctx.Err() == nil {
			c.solarErr, c.solarErrAt = err, time.Now()
		}
		if haveLast {
			return c.solar, nil
		}
		return config.SolarTimes{}, err
	}
	c.solar, c.solarAt = s, time.Now()
	c.solarErr, c.solarErrAt = nil, time.Time{}
	return s, nil
}

// fetchSolar reads sun.sun. Its attributes are the *next* occurrence of each
// event, which may fall tomorrow — but only the time of day is used, and that
// drifts by about a minute between consecutive days, so tomorrow's value is a
// fine stand-in.
func (c *Cache) fetchSolar(ctx context.Context) (config.SolarTimes, error) {
	body, err := c.client.EntityState(ctx, "sun.sun")
	if err != nil {
		return config.SolarTimes{}, err
	}
	var payload struct {
		Attributes struct {
			NextDawn    time.Time `json:"next_dawn"`
			NextDusk    time.Time `json:"next_dusk"`
			NextRising  time.Time `json:"next_rising"`
			NextSetting time.Time `json:"next_setting"`
		} `json:"attributes"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return config.SolarTimes{}, fmt.Errorf("hasscache: parse sun.sun: %w", err)
	}
	a := payload.Attributes
	if a.NextDawn.IsZero() || a.NextDusk.IsZero() || a.NextRising.IsZero() || a.NextSetting.IsZero() {
		return config.SolarTimes{}, fmt.Errorf("hasscache: sun.sun is missing solar attributes")
	}
	return config.SolarTimes{
		Dawn:    timeOfDay(a.NextDawn, c.loc),
		Dusk:    timeOfDay(a.NextDusk, c.loc),
		Sunrise: timeOfDay(a.NextRising, c.loc),
		Sunset:  timeOfDay(a.NextSetting, c.loc),
	}, nil
}

func timeOfDay(t time.Time, loc *time.Location) time.Duration {
	local := t.In(loc)
	return time.Duration(local.Hour())*time.Hour +
		time.Duration(local.Minute())*time.Minute +
		time.Duration(local.Second())*time.Second
}
