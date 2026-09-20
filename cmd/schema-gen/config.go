package main

import jsonschema "github.com/swaggest/jsonschema-go"

// These types mirror internal/config by hand; keep them in sync.

// duration is a time.ParseDuration string; YAML lets an unquoted 0 through as
// an integer.
type duration string

const durationPattern = `^([-+]?(([0-9]+(\.[0-9]*)?|\.[0-9]+)(ns|us|µs|μs|ms|s|m|h))+|[-+]?0)?$`

func (duration) PrepareJSONSchema(s *jsonschema.Schema) error {
	s.Type = &jsonschema.Type{SliceOfSimpleTypeValues: []jsonschema.SimpleType{jsonschema.String, jsonschema.Integer}}
	s.WithPattern(durationPattern)
	return nil
}

type hassConfig struct {
	URL   string `json:"url" required:"true" description:"Home Assistant base URL, e.g. https://homeassistant.example.com"`
	Token string `json:"token" required:"true" description:"Home Assistant long-lived access token"`
}

type slackConfig struct {
	BotToken string `json:"botToken" required:"true" description:"Slack bot token (xoxb-...) with chat:write. A bot rather than an incoming webhook, because only the bot that posted a message can edit it in place. Invite the bot to each channel, or grant chat:write.public."`
}

type ntfyConfig struct {
	URL   string `json:"url" required:"true" description:"ntfy server base URL. Updating a notification in place needs server >= 2.16."`
	Token string `json:"token" description:"ntfy access token. Omit for open topics."`
}

type mqttConfig struct {
	Broker      string `json:"broker" required:"true" description:"paho MQTT broker URI, e.g. tcp://mqtt.example.svc.cluster.local:1883"`
	ClientID    string `json:"clientId" description:"MQTT client ID (default: frigate-notify)"`
	Username    string `json:"username" required:"true" description:"MQTT username"`
	Password    string `json:"password" required:"true" description:"MQTT password"`
	TopicPrefix string `json:"topicPrefix" description:"Frigate MQTT topic_prefix (default: frigate)"`
}

type redisConfig struct {
	URL string `json:"url" description:"Valkey/Redis URL for cooldowns and review state. Omit to keep state in the db (or in memory when there is none), which for memory does not survive a restart."`
}

type dbConfig struct {
	URL string `json:"url" description:"MongoDB or PostgreSQL URL (mongodb://, mongodb+srv://, postgres://, postgresql://) for audit persistence of events/notifications, and for cooldown/review state when redis.url is unset. Omit to skip persistence entirely."`
}

type mediaConfig struct {
	FrigateURL    string   `json:"frigateURL" description:"In-cluster Frigate base URL (the unauthenticated listener, port 5000)"`
	PublicBaseURL string   `json:"publicBaseURL" description:"Public base URL of this service's media proxy, e.g. https://frigate-notifications.example.com"`
	SigningKey    string   `json:"signingKey" pattern:"^(\\s*([0-9a-fA-F]{2}){16,}\\s*)?$" description:"Hex-encoded HMAC key (at least 32 hex chars) for signed media links. Rotating it invalidates every outstanding link."`
	LinkTTL       duration `json:"linkTTL" description:"How long a signed media link stays valid, e.g. 24h (default: 24h)"`
}

type window struct {
	From string `json:"from" required:"true" description:"Start of the window: \"HH:MM\" (quote it), a 12-hour time like 9am or 11:30pm, noon, midnight, or a solar event — sunrise, sunset, dawn, dusk — with an optional offset, e.g. dusk+30m"`
	To   string `json:"to" required:"true" description:"End of the window, same grammar as from. Earlier than from means the window wraps midnight."`
}

// targetType is a string with a fixed value set, so the enum lands on the
// field itself.
type targetType string

func (targetType) Enum() []any { return []any{"hass", "slack", "ntfy", "discord"} }

type target struct {
	Type       targetType `json:"type" required:"true" description:"Which backend delivers to this target"`
	Service    string     `json:"service" description:"hass: bare suffix of notify.<service>, e.g. mobile_app_alice_phone. Required for hass."`
	Channel    string     `json:"channel" description:"slack: a channel id (C...), or a user id (U...) to DM. Not a channel name. Required for slack."`
	Topic      string     `json:"topic" description:"ntfy: topic to publish to. Required for ntfy."`
	WebhookURL string     `json:"webhookURL" description:"discord: the channel's https webhook URL. It is the whole credential; treat it as a secret. Required for discord."`
}

type recipient struct {
	Targets       []target `json:"targets" required:"true" minItems:"1" description:"Every place this recipient is notified. The policy below applies once per recipient, then every target gets the notification."`
	AllowCritical bool     `json:"allowCritical" description:"Allow critical rules to bypass this recipient's silent/do-not-disturb mode. On backends with no such bypass, critical is styling only."`
	ActiveHours   *window  `json:"activeHours" description:"Non-critical notifications are sent only inside this window; outside it, only critical ones get through"`
	MaxPerHour    int      `json:"maxPerHour" minimum:"0" description:"Beyond this many notifications per hour, collapse into a rolling digest (0 = no cap)"`
}

type camera struct {
	FriendlyName   string `json:"friendlyName" description:"Notification title for this camera (default: the camera key)"`
	LiveViewEntity string `json:"liveViewEntity" description:"camera.<x> entity ID, required by the liveview preset"`
}

// severity is a string with a fixed value set; as a named type the enum lands
// on the array's items rather than on the array itself.
type severity string

func (severity) Enum() []any { return []any{"alert", "detection"} }

type ruleConditions struct {
	Cameras          []string          `json:"cameras" description:"Frigate camera names"`
	Labels           []string          `json:"labels" description:"Object labels, matched against the review's objects"`
	Zones            []string          `json:"zones" description:"Zones the review touched"`
	SubLabels        []string          `json:"subLabels" description:"Recognized faces that must be present"`
	ExcludeSubLabels []string          `json:"excludeSubLabels" description:"Recognized faces that suppress the rule, e.g. household members. Only suppresses when every detected person is recognized and excluded; an unrecognized or non-excluded person alongside them still matches"`
	Severity         []severity        `json:"severity" description:"Frigate review severity"`
	Hours            *window           `json:"hours" description:"Time-of-day window this rule applies in"`
	MinDwell         duration          `json:"minDwell" description:"Object must persist at least this long, e.g. 15s"`
	EntityState      map[string]string `json:"entityState" description:"Home Assistant entity ID to required state"`
}

// unlessEntry needs at least one condition: an empty entry would suppress the
// rule entirely.
type unlessEntry ruleConditions

type rule struct {
	Name          string         `json:"name" required:"true" minLength:"1" description:"Unique rule name; appears in metrics and the decision trace"`
	When          ruleConditions `json:"when" required:"true" description:"Conditions, all of which must hold"`
	Unless        []unlessEntry  `json:"unless" description:"Suppress the rule when ANY of these entries matches"`
	Holdoff       duration       `json:"holdoff" description:"Delay the decision so late signals (face recognition) arrive first, e.g. 5s"`
	Cooldown      duration       `json:"cooldown" description:"Minimum time between first sends, e.g. 30s"`
	CooldownScope string         `json:"cooldownScope" enum:"camera,rule,global" description:"What the cooldown is counted against (default: camera)"`
	To            []string       `json:"to" required:"true" minItems:"1" description:"Recipient names to notify"`
	Preset        string         `json:"preset" enum:"auto,text,liveview" description:"auto (default): best media that exists and fits, else text. text: no media. liveview: auto plus an iOS live camera stream"`
	Critical      bool           `json:"critical" description:"Send as a critical push that breaks through silent mode"`
	Timezone      string         `json:"timezone" description:"IANA timezone override for this rule's hours window (default: the top-level timezone)"`
}

type config struct {
	Hass          hassConfig           `json:"hass"`
	Slack         *slackConfig         `json:"slack" description:"Needed by recipients with a slack target"`
	Ntfy          *ntfyConfig          `json:"ntfy" description:"Needed by recipients with an ntfy target"`
	MQTT          mqttConfig           `json:"mqtt"`
	Redis         redisConfig          `json:"redis"`
	DB            dbConfig             `json:"db"`
	Media         mediaConfig          `json:"media"`
	Timezone      string               `json:"timezone" description:"IANA timezone for all wall-clock comparisons (default: Local)"`
	DashboardPath string               `json:"dashboardPath" description:"Home Assistant path a notification tap opens (hass targets)"`
	DashboardURL  string               `json:"dashboardURL" pattern:"^(https?://.+)?$" description:"Absolute http(s) URL of the same dashboard, for backends that can't resolve a relative path"`
	DryRun        bool                 `json:"dryRun" description:"Build and log notifications without sending them"`
	Recipients    map[string]recipient `json:"recipients" minProperties:"1" description:"Keyed by recipient name, referenced by rules[].to. At least one is required."`
	Cameras       map[string]camera    `json:"cameras" description:"Keyed by Frigate camera name"`
	Rules         []rule               `json:"rules" minItems:"1" description:"Ordered; the first matching rule wins, and position is priority. At least one is required."`
	LogLevel      string               `json:"logLevel" description:"trace/debug/info/warn/error (default: info)"`
}
