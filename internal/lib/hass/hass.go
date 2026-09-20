package hass

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"time"

	otel_bootstrap "github.com/ConnorsApps/frigate-notifications/internal/lib/otel"
	"github.com/go-resty/resty/v2"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel"
	attr "go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

func newSpan(ctx context.Context, name string, attributes ...attr.KeyValue) (context.Context, trace.Span) {
	return otel.Tracer("hass").Start(ctx, name, trace.WithAttributes(attributes...))
}

type Client struct {
	resty  *resty.Client
	logger zerolog.Logger
}

func New(url, token string) *Client {
	return &Client{
		resty: resty.New().
			SetBaseURL(url).
			SetRetryCount(3).
			SetRetryWaitTime(2 * time.Second).
			SetAuthToken(token).
			SetTransport(otel_bootstrap.NewHTTPClient().Transport),
		logger: log.With().Str("logger", "hass").Logger(),
	}
}

func httpIsOk(code int) bool {
	return code >= 200 && code < 300
}

func (c *Client) callService(ctx context.Context, domain, method string, body any) error {
	resp, err := c.resty.R().
		SetHeader("Content-Type", "application/json").
		SetBody(body).
		SetContext(ctx).
		Post(fmt.Sprintf("api/services/%s/%s", domain, method))

	if err != nil || !httpIsOk(resp.StatusCode()) {
		c.logger.Error().Err(err).
			Any("body", body).
			Int("statusCode", resp.StatusCode()).
			Str("domain", domain).
			Str("method", method).
			Msg("bad status code for home assistant request")
		if err != nil {
			return fmt.Errorf("hass request %s/%s: %w", domain, method, err)
		}
		// No transport error: the request reached Home Assistant and it
		// rejected the call. Carry the status and body so the caller's log
		// line isn't just "%!w(<nil>)".
		return fmt.Errorf("hass request %s/%s: status %d: %s", domain, method, resp.StatusCode(), resp.String())
	}
	return nil
}

// EntityState fetches the raw JSON state for the given entity ID.
func (c *Client) EntityState(ctx context.Context, entityId string) ([]byte, error) {
	resp, err := c.resty.R().
		SetContext(ctx).
		Get("api/states/" + entityId)

	if err != nil {
		c.logger.Err(err).
			Int("statusCode", resp.StatusCode()).
			Str("entityId", entityId).
			Msg("error for get state request")
		return nil, fmt.Errorf("hass entity state %s: %w", entityId, err)
	}

	if !httpIsOk(resp.StatusCode()) {
		resBody := resp.String()

		c.logger.Error().
			Int("statusCode", resp.StatusCode()).
			Str("entityId", entityId).
			Str("response", resBody).
			Msg("bad status code for get state request")
		return nil, fmt.Errorf("hass entity state %s: %s", entityId, resBody)
	}
	return resp.Body(), nil
}

// Notify calls a notify.<service> Home Assistant service (e.g. a
// notify.mobile_app_xxx target). data is passed through as the service call
// body (typically at least a "message" key).
func (c *Client) Notify(ctx context.Context, service string, data map[string]any) error {
	ctx, span := newSpan(ctx, "hass_notify", attr.String("service", service))
	defer span.End()

	if err := c.callService(ctx, "notify", service, data); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "unable to send notification")
		return err
	}
	return nil
}

// NotifyServices returns the set of notify.<service> suffixes Home Assistant
// currently exposes. Useful at startup: a misspelled service name otherwise
// fails silently on every notification.
func (c *Client) NotifyServices(ctx context.Context) (map[string]bool, error) {
	resp, err := c.resty.R().SetContext(ctx).Get("api/services")
	if err != nil {
		return nil, fmt.Errorf("hass services: %w", err)
	}
	if !httpIsOk(resp.StatusCode()) {
		return nil, fmt.Errorf("hass services: %s", resp.String())
	}

	var domains []struct {
		Domain   string         `json:"domain"`
		Services map[string]any `json:"services"`
	}
	if err := json.Unmarshal(resp.Body(), &domains); err != nil {
		return nil, fmt.Errorf("hass services unmarshal: %w", err)
	}

	services := map[string]bool{}
	for _, d := range domains {
		if d.Domain != "notify" {
			continue
		}
		for name := range d.Services {
			services[name] = true
		}
	}
	return services, nil
}
