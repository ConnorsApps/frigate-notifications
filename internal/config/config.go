package config

import (
	"fmt"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ConnorsApps/frigate-notifications/internal/db"
	"github.com/caarlos0/env/v11"
	"github.com/goccy/go-yaml"
	"github.com/rs/zerolog/log"
)

type HassConfig struct {
	URL   string `yaml:"url" env:"HASS_URL,notEmpty"`
	Token string `yaml:"token" env:"HASS_TOKEN,notEmpty"`
}

// SlackConfig is a bot token: only the posting bot can edit its message.
type SlackConfig struct {
	BotToken string `yaml:"botToken" env:"SLACK_BOT_TOKEN"`
}

// NtfyConfig points at an ntfy server; Token is optional.
type NtfyConfig struct {
	URL   string `yaml:"url" env:"NTFY_URL"`
	Token string `yaml:"token" env:"NTFY_TOKEN"`
}

type MQTTConfig struct {
	// Broker: paho.mqtt.golang server URI, e.g. "tcp://host:1883".
	Broker      string `yaml:"broker" env:"MQTT_BROKER,notEmpty"`
	ClientID    string `yaml:"clientId" env:"MQTT_CLIENT_ID" envDefault:"frigate-notify"`
	Username    string `yaml:"username" env:"MQTT_USERNAME,notEmpty"`
	Password    string `yaml:"password" env:"MQTT_PASSWORD,notEmpty"`
	TopicPrefix string `yaml:"topicPrefix" env:"MQTT_TOPIC_PREFIX" envDefault:"frigate"`
}

// RedisConfig points at the per-app Valkey. Optional: with no URL the store
// runs entirely in process memory, which is correct but loses state across
// restarts (and CI restarts this deployment on every image push).
type RedisConfig struct {
	URL string `yaml:"url" env:"REDIS_URL"`
}

// DBConfig points at MongoDB or PostgreSQL (by URL scheme): the audit log,
// plus decision state when no Redis URL is set. Optional: without it audit
// persistence is skipped, and it never gates startup or a send.
type DBConfig struct {
	URL string `yaml:"url" env:"DB_URL"`
}

// MediaConfig configures the signed media proxy that lets phones off the LAN
// fetch snapshots and clips from Frigate's unauthenticated in-cluster port.
type MediaConfig struct {
	// FrigateURL: in-cluster base URL, the no-auth listener (port 5000).
	FrigateURL string `yaml:"frigateURL" env:"MEDIA_FRIGATE_URL"`
	// PublicBaseURL: what phones resolve, e.g. https://frigate-notifications.example.com.
	PublicBaseURL string `yaml:"publicBaseURL" env:"MEDIA_PUBLIC_BASE_URL"`
	// SigningKey: hex. Rotating it invalidates every outstanding link.
	SigningKey string `yaml:"signingKey" env:"MEDIA_SIGNING_KEY"`
	// LinkTTL: how long a signed URL stays valid. Long enough that a
	// notification opened the next morning still renders its image.
	LinkTTL Duration `yaml:"linkTTL"`
}

// Enabled reports whether media links can be built. With it off, presets
// degrade to text rather than emitting links that would 403.
func (m MediaConfig) Enabled() bool {
	return m.FrigateURL != "" && m.PublicBaseURL != "" && m.SigningKey != ""
}

// TargetType names a notification backend.
type TargetType string

const (
	TargetHass    TargetType = "hass"
	TargetSlack   TargetType = "slack"
	TargetNtfy    TargetType = "ntfy"
	TargetDiscord TargetType = "discord"
)

// Target is one place a recipient is notified. Flat, so strict YAML still
// rejects typos; validate checks which fields Type needs.
type Target struct {
	Type TargetType `yaml:"type"`
	// Service (hass): bare suffix of notify.<service>, e.g. "mobile_app_pixel".
	Service string `yaml:"service"`
	// Channel (slack): a channel or user id (DM), not a name.
	Channel string `yaml:"channel"`
	// Topic (ntfy).
	Topic string `yaml:"topic"`
	// WebhookURL (discord): itself the secret.
	WebhookURL string `yaml:"webhookURL"`
}

// String names the target for logs without leaking the webhook token.
func (t Target) String() string {
	switch t.Type {
	case TargetHass:
		return "hass:" + t.Service
	case TargetSlack:
		return "slack:" + t.Channel
	case TargetNtfy:
		return "ntfy:" + t.Topic
	case TargetDiscord:
		return "discord:" + webhookID(t.WebhookURL)
	}
	return string(t.Type)
}

// webhookID is the <id> of .../webhooks/<id>/<token>, or "" if not that shape.
func webhookID(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, p := range parts {
		if p == "webhooks" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// Recipient is a person. Rules name recipients rather than backends so
// per-person policy lives in one place instead of being duplicated across a
// rule per person, and a person can be reached on several backends at once.
type Recipient struct {
	// Targets: policy below applies once per recipient, not per target.
	Targets []Target `yaml:"targets"`
	// AllowCritical false downgrades a critical rule to a normal push.
	AllowCritical bool `yaml:"allowCritical"`
	// ActiveHours: non-critical pushes are sent only inside this window.
	// Named for what it permits rather than what it blocks — the inverse
	// reading ("quiet hours") inverts the meaning of every value in it.
	// Unset means always awake.
	ActiveHours *Window `yaml:"activeHours"`
	// MaxPerHour: beyond this, pushes collapse into a rolling digest. 0 = no cap.
	MaxPerHour int `yaml:"maxPerHour"`
}

// Camera maps a Frigate camera name to presentation details.
type Camera struct {
	// FriendlyName: notification title. Defaults to the camera key.
	FriendlyName string `yaml:"friendlyName"`
	// LiveViewEntity: camera.<x> entity for the liveview preset.
	LiveViewEntity string `yaml:"liveViewEntity"`
}

// RuleConditions is shared by both `when` and `unless` so the two can never
// drift apart. Fields are ANDed; each list is an OR; omitted means "any".
type RuleConditions struct {
	Cameras []string `yaml:"cameras"`
	// Labels matches review data.objects.
	Labels []string `yaml:"labels"`
	// Zones matches review data.zones.
	Zones []string `yaml:"zones"`
	// SubLabels / ExcludeSubLabels match recognized faces (review data.sub_labels).
	// ExcludeSubLabels suppresses only when it accounts for every detected person.
	SubLabels        []string `yaml:"subLabels"`
	ExcludeSubLabels []string `yaml:"excludeSubLabels"`
	// Severity: "alert" and/or "detection".
	Severity []string `yaml:"severity"`
	Hours    *Window  `yaml:"hours"`
	// MinDwell requires the object to persist this long.
	MinDwell Duration `yaml:"minDwell"`
	// EntityState maps a Home Assistant entity ID to its required state.
	EntityState map[string]string `yaml:"entityState"`
}

// IsZero reports whether no condition at all is set.
func (c RuleConditions) IsZero() bool {
	return len(c.Cameras) == 0 && len(c.Labels) == 0 && len(c.Zones) == 0 &&
		len(c.SubLabels) == 0 && len(c.ExcludeSubLabels) == 0 &&
		len(c.Severity) == 0 && c.Hours == nil && c.MinDwell == 0 && len(c.EntityState) == 0
}

// Preset selects the media shape of a notification.
type Preset string

const (
	// PresetAuto attaches the best media that exists and fits at each phase,
	// falling back to lighter kinds and finally to text.
	PresetAuto Preset = "auto"
	// PresetText attaches no media.
	PresetText Preset = "text"
	// PresetLiveView is auto plus an iOS live camera stream on expand.
	PresetLiveView Preset = "liveview"
)

// CooldownScope selects the key a rule's cooldown is counted against.
type CooldownScope string

const (
	// ScopeCamera: one cooldown per camera per rule.
	ScopeCamera CooldownScope = "camera"
	// ScopeRule: one cooldown across every camera the rule covers, so one
	// person walking front_porch -> garage yields a single push.
	ScopeRule CooldownScope = "rule"
	// ScopeGlobal: one cooldown across all rules.
	ScopeGlobal CooldownScope = "global"
)

// Rule is one entry in the ordered rule list. Position is priority: the first
// match wins, and an end-phase re-match may only escalate to a lower index.
type Rule struct {
	Name string         `yaml:"name"`
	When RuleConditions `yaml:"when"`
	// Unless suppresses the rule when ANY entry matches. It is a list because
	// conditions within one entry are ANDed, and the common case ("we're home,
	// OR there's a guest") needs an OR.
	Unless []RuleConditions `yaml:"unless"`
	// Holdoff delays the decision so late signals (notably face recognition,
	// which lands after the "new" payload) arrive first. A push can't be
	// recalled, so rules using ExcludeSubLabels default to defaultHoldoff.
	Holdoff       *Duration     `yaml:"holdoff"`
	Cooldown      Duration      `yaml:"cooldown"`
	CooldownScope CooldownScope `yaml:"cooldownScope"`
	To            []string      `yaml:"to"`
	Preset        Preset        `yaml:"preset"`
	Critical      bool          `yaml:"critical"`
	// Timezone overrides the top-level timezone for this rule's hours
	// windows. Empty means use the config-wide timezone.
	Timezone string `yaml:"timezone"`
	// Location is Timezone (or the config-wide Location) resolved at load.
	Location *time.Location `yaml:"-"`
}

// defaultHoldoff waits for Frigate's face recognition to populate sub_labels,
// which usually lands a few seconds after "new".
const defaultHoldoff = 5 * time.Second

// EffectiveHoldoff is the configured holdoff, or the face-recognition
// default for any rule that can match a person.
func (r Rule) EffectiveHoldoff() time.Duration {
	if r.Holdoff != nil {
		return time.Duration(*r.Holdoff)
	}
	if len(r.When.ExcludeSubLabels) > 0 || len(r.When.SubLabels) > 0 || slices.Contains(r.When.Labels, "person") {
		return defaultHoldoff
	}
	return 0
}

type Config struct {
	Hass  HassConfig  `yaml:"hass"`
	Slack SlackConfig `yaml:"slack"`
	Ntfy  NtfyConfig  `yaml:"ntfy"`
	MQTT  MQTTConfig  `yaml:"mqtt"`
	Redis RedisConfig `yaml:"redis"`
	DB    DBConfig    `yaml:"db"`
	Media MediaConfig `yaml:"media"`

	// Timezone: IANA name for all wall-clock comparisons.
	Timezone string `yaml:"timezone" env:"TIMEZONE" envDefault:"Local"`
	// DashboardPath: Home Assistant path a notification tap opens.
	DashboardPath string `yaml:"dashboardPath"`
	// DashboardURL: absolute link to the same dashboard, for backends that
	// aren't the Home Assistant app and so can't resolve a relative path.
	DashboardURL string `yaml:"dashboardURL"`
	// DryRun builds and logs the full payload without calling Home Assistant.
	DryRun bool `yaml:"dryRun"`

	Recipients map[string]Recipient `yaml:"recipients"`
	Cameras    map[string]Camera    `yaml:"cameras"`
	Rules      []Rule               `yaml:"rules"`

	LogLevel string `yaml:"logLevel"`

	// Location is Timezone resolved at load.
	Location *time.Location `yaml:"-"`
}

// CameraName returns a camera's display name, falling back to its key.
func (c *Config) CameraName(camera string) string {
	if cam, ok := c.Cameras[camera]; ok && cam.FriendlyName != "" {
		return cam.FriendlyName
	}
	return camera
}

func MustRead(path string) *Config {
	c, err := Read(path)
	if err != nil {
		log.Fatal().Str("path", path).Err(err).Msg("invalid config")
	}
	return c
}

// Read loads, defaults, and validates the config file.
func Read(path string) (*Config, error) {
	if path == "" {
		path = "config.yaml"
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	c := &Config{}
	// Strict: an unknown key is a typo, and a typo'd condition (`sevrity:`)
	// would otherwise silently widen a rule to match everything.
	if err := yaml.UnmarshalWithOptions(data, c, yaml.DisallowUnknownField()); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Run the yaml-loaded values through caarlos0/env's tag logic: ",notEmpty"
	// fields must be set, and "envDefault" fills in blanks.
	if err := env.ParseWithOptions(c, env.Options{Environment: envFor(c)}); err != nil {
		return nil, err
	}

	if err := c.applyDefaults(); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// envFor is the environment Read hands to caarlos0/env: each field's yaml
// value, overridden by a non-empty process variable of the same name, so the
// chart and the Home Assistant app can inject secrets. Hand-written per field:
// a new env-tagged field needs an entry or its tag does nothing.
func envFor(c *Config) map[string]string {
	vars := map[string]string{
		"HASS_URL":              c.Hass.URL,
		"HASS_TOKEN":            c.Hass.Token,
		"SLACK_BOT_TOKEN":       c.Slack.BotToken,
		"NTFY_URL":              c.Ntfy.URL,
		"NTFY_TOKEN":            c.Ntfy.Token,
		"MQTT_BROKER":           c.MQTT.Broker,
		"MQTT_CLIENT_ID":        c.MQTT.ClientID,
		"MQTT_USERNAME":         c.MQTT.Username,
		"MQTT_PASSWORD":         c.MQTT.Password,
		"MQTT_TOPIC_PREFIX":     c.MQTT.TopicPrefix,
		"REDIS_URL":             c.Redis.URL,
		"DB_URL":                c.DB.URL,
		"MEDIA_FRIGATE_URL":     c.Media.FrigateURL,
		"MEDIA_PUBLIC_BASE_URL": c.Media.PublicBaseURL,
		"MEDIA_SIGNING_KEY":     c.Media.SigningKey,
		"TIMEZONE":              c.Timezone,
	}
	for name := range vars {
		if v := os.Getenv(name); v != "" {
			vars[name] = v
		}
	}
	return vars
}

func (c *Config) applyDefaults() error {
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return fmt.Errorf("timezone %q: %w", c.Timezone, err)
	}
	c.Location = loc

	if c.Media.LinkTTL == 0 {
		c.Media.LinkTTL = Duration(24 * time.Hour)
	}

	for i := range c.Rules {
		r := &c.Rules[i]
		if r.Preset == "" {
			r.Preset = PresetAuto
		}
		if r.CooldownScope == "" {
			r.CooldownScope = ScopeCamera
		}
		r.Location = c.Location
		if r.Timezone != "" {
			if r.Location, err = time.LoadLocation(r.Timezone); err != nil {
				return fmt.Errorf("rules[%d].timezone %q: %w", i, r.Timezone, err)
			}
		}
	}
	return nil
}

func (c *Config) validate() error {
	var errs []string
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}

	if c.DB.URL != "" {
		if _, err := db.KindOf(c.DB.URL); err != nil {
			add("db.url: %v", err)
		}
	}

	if len(c.Recipients) == 0 {
		add("recipients: at least one recipient is required")
	}
	for name, r := range c.Recipients {
		if len(r.Targets) == 0 {
			add("recipients.%s.targets: at least one target is required", name)
		}
		for i, t := range r.Targets {
			c.validateTarget(fmt.Sprintf("recipients.%s.targets[%d]", name, i), t, add)
		}
		if r.MaxPerHour < 0 {
			add("recipients.%s.maxPerHour must not be negative", name)
		}
		validateWindow(fmt.Sprintf("recipients.%s.activeHours", name), r.ActiveHours, add)
	}

	if c.DashboardURL != "" {
		if u, err := url.Parse(c.DashboardURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			add("dashboardURL: want an absolute http(s) URL, got %q", c.DashboardURL)
		}
	}

	if len(c.Rules) == 0 {
		add("rules: at least one rule is required")
	}
	seenNames := map[string]bool{}
	for i, rule := range c.Rules {
		where := fmt.Sprintf("rules[%d]", i)
		if rule.Name == "" {
			add("%s.name is required", where)
		} else {
			where = fmt.Sprintf("rules[%d] (%s)", i, rule.Name)
			if seenNames[rule.Name] {
				// Names key metrics and the decision trace; duplicates make both unreadable.
				add("%s: duplicate rule name", where)
			}
			seenNames[rule.Name] = true
		}

		if len(rule.To) == 0 {
			add("%s.to is required", where)
		}
		for _, to := range rule.To {
			if _, ok := c.Recipients[to]; !ok {
				add("%s.to: unknown recipient %q", where, to)
			}
		}

		switch rule.Preset {
		case PresetAuto, PresetText, PresetLiveView:
		default:
			add("%s.preset: unknown preset %q (want auto, text, or liveview)", where, rule.Preset)
		}

		switch rule.CooldownScope {
		case ScopeCamera, ScopeRule, ScopeGlobal:
		default:
			add("%s.cooldownScope: unknown scope %q (want camera, rule, or global)", where, rule.CooldownScope)
		}

		c.validateConditions(where+".when", rule.When, add)
		for j, u := range rule.Unless {
			if u.IsZero() {
				add("%s.unless[%d]: empty — an empty unless entry would suppress the rule entirely", where, j)
			}
			c.validateConditions(fmt.Sprintf("%s.unless[%d]", where, j), u, add)
		}

		if rule.Preset == PresetLiveView {
			// With no camera list the rule applies to every camera, so every
			// camera needs an entity — checking only the named ones would skip
			// the check entirely for exactly the broadest rules.
			cams := rule.When.Cameras
			if len(cams) == 0 {
				for name := range c.Cameras {
					cams = append(cams, name)
				}
				sort.Strings(cams)
			}
			for _, cam := range cams {
				if c.Cameras[cam].LiveViewEntity == "" {
					add("%s: preset liveview needs cameras.%s.liveViewEntity", where, cam)
				}
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("config:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// validateTarget checks a target's fields and that its backend is configured.
func (c *Config) validateTarget(where string, t Target, add func(string, ...any)) {
	switch t.Type {
	case TargetHass:
		if t.Service == "" {
			add("%s.service is required for a hass target", where)
		}
		if strings.HasPrefix(t.Service, "notify.") {
			add("%s.service should be the bare service suffix, not %q", where, t.Service)
		}
	case TargetSlack:
		if t.Channel == "" {
			add("%s.channel is required for a slack target", where)
		}
		if c.Slack.BotToken == "" {
			add("%s: slack.botToken is required to use a slack target", where)
		}
	case TargetNtfy:
		if t.Topic == "" {
			add("%s.topic is required for an ntfy target", where)
		}
		if c.Ntfy.URL == "" {
			add("%s: ntfy.url is required to use an ntfy target", where)
		}
	case TargetDiscord:
		if u, err := url.Parse(t.WebhookURL); err != nil || u.Scheme != "https" || u.Host == "" {
			add("%s.webhookURL: want an https webhook URL", where)
		}
	default:
		add("%s.type: unknown type %q (want hass, slack, ntfy, or discord)", where, t.Type)
	}
}

func (c *Config) validateConditions(where string, cond RuleConditions, add func(string, ...any)) {
	for _, cam := range cond.Cameras {
		if _, ok := c.Cameras[cam]; !ok {
			add("%s.cameras: unknown camera %q", where, cam)
		}
	}
	for _, s := range cond.Severity {
		if s != "alert" && s != "detection" {
			add("%s.severity: unknown severity %q (want alert or detection)", where, s)
		}
	}
	validateWindow(where+".hours", cond.Hours, add)
}

// validateWindow rejects a half-specified window. A missing endpoint parses as
// the zero value, i.e. midnight, so `{from: "22:00"}` would silently mean
// 22:00 to 00:00 rather than erroring.
func validateWindow(where string, w *Window, add func(string, ...any)) {
	if w == nil {
		return
	}
	if w.From.String() == "" || w.To.String() == "" {
		add("%s: both from and to are required", where)
	}
}
