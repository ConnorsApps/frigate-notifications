package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig materializes a config file for Read.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimalConfig = `
hass: { url: http://hass, token: t }
mqtt: { broker: tcp://b:1883, username: u, password: p }
timezone: America/New_York
recipients:
  alice: { targets: [{ type: hass, service: mobile_app_phone }], allowCritical: true }
cameras:
  front_porch: { friendlyName: Front Porch, liveViewEntity: camera.front_porch }
rules:
  - name: day
    when: { hours: { from: "06:00", to: "22:00" }, labels: [person] }
    cooldown: 30s
    to: [alice]
    preset: auto
`

func TestReadMinimal(t *testing.T) {
	cfg, err := Read(writeConfig(t, minimalConfig))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if cfg.Location.String() != "America/New_York" {
		t.Errorf("Location = %s, want America/New_York", cfg.Location)
	}
	if got := cfg.Rules[0].Cooldown; got.String() != "30s" {
		t.Errorf("cooldown = %s, want 30s", got)
	}
	if got := cfg.Rules[0].CooldownScope; got != ScopeCamera {
		t.Errorf("cooldownScope = %q, want the camera default", got)
	}
	if got := cfg.Media.LinkTTL.String(); got != "24h0m0s" {
		t.Errorf("linkTTL = %s, want the 24h default", got)
	}
	if cfg.CameraName("front_porch") != "Front Porch" {
		t.Error("CameraName should use friendlyName")
	}
	if cfg.CameraName("garage") != "garage" {
		t.Error("CameraName should fall back to the camera key")
	}
	if cfg.Rules[0].Location != cfg.Location {
		t.Errorf("rule with no timezone override should default to the config-wide Location")
	}
}

// A rule's timezone overrides the config-wide one for that rule's hours
// windows only; other rules keep using the config-wide Location.
func TestRuleTimezoneOverride(t *testing.T) {
	body := strings.Replace(minimalConfig, "  - name: day\n", "  - name: day\n    timezone: America/Los_Angeles\n", 1)
	cfg, err := Read(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := cfg.Rules[0].Location.String(); got != "America/Los_Angeles" {
		t.Errorf("Rules[0].Location = %s, want America/Los_Angeles", got)
	}
	if cfg.Location.String() != "America/New_York" {
		t.Errorf("config-wide Location should be unaffected by a rule override, got %s", cfg.Location)
	}
}

// A typo in a condition name must fail loudly. Silently ignoring it would
// widen the rule to match everything, which is the worst possible outcome for
// a rule whose job is to be selective.
func TestReadRejectsUnknownFields(t *testing.T) {
	body := strings.Replace(minimalConfig, "labels: [person]", "lables: [person]", 1)
	_, err := Read(writeConfig(t, body))
	if err == nil {
		t.Fatal("Read accepted an unknown field")
	}
	if !strings.Contains(err.Error(), "lables") {
		t.Errorf("error should name the offending field, got: %v", err)
	}
}

// One recipient can be reached on several backends at once, each with its
// own required fields.
func TestRecipientWithSeveralBackends(t *testing.T) {
	body := strings.Replace(minimalConfig,
		"targets: [{ type: hass, service: mobile_app_phone }]",
		`targets:
      - { type: hass, service: mobile_app_phone }
      - { type: slack, channel: U0123 }
      - { type: ntfy, topic: frigate }
      - { type: discord, webhookURL: "https://discord.com/api/webhooks/1/x" }`, 1)
	body = "slack: { botToken: xoxb-x }\nntfy: { url: 'https://ntfy.test' }\n" + body

	cfg, err := Read(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	targets := cfg.Recipients["alice"].Targets
	if len(targets) != 4 {
		t.Fatalf("targets = %d, want 4", len(targets))
	}
	// The discord webhook token is the whole credential and must not reach logs.
	if got := targets[3].String(); strings.Contains(got, "webhooks") {
		t.Errorf("Target.String() = %q leaks the webhook URL", got)
	}
}

func TestValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(string) string
		wantMsg string
	}{
		{
			name:    "unknown recipient",
			mutate:  func(s string) string { return strings.Replace(s, "to: [alice]", "to: [nobody]", 1) },
			wantMsg: `unknown recipient "nobody"`,
		},
		{
			name:    "unknown camera in conditions",
			mutate:  func(s string) string { return strings.Replace(s, "labels: [person]", "cameras: [kitchen]", 1) },
			wantMsg: `unknown camera "kitchen"`,
		},
		{
			name:    "unknown preset",
			mutate:  func(s string) string { return strings.Replace(s, "preset: auto", "preset: hologram", 1) },
			wantMsg: `unknown preset "hologram"`,
		},
		{
			name:    "unknown severity",
			mutate:  func(s string) string { return strings.Replace(s, "labels: [person]", "severity: [urgent]", 1) },
			wantMsg: `unknown severity "urgent"`,
		},
		{
			name:    "bad time endpoint",
			mutate:  func(s string) string { return strings.Replace(s, `from: "06:00"`, `from: "lunchtime"`, 1) },
			wantMsg: "unrecognized time",
		},
		{
			name: "recipient with no targets",
			mutate: func(s string) string {
				return strings.Replace(s, "targets: [{ type: hass, service: mobile_app_phone }]", "targets: []", 1)
			},
			wantMsg: "at least one target is required",
		},
		{
			name:    "unknown target type",
			mutate:  func(s string) string { return strings.Replace(s, "type: hass", "type: carrier-pigeon", 1) },
			wantMsg: `unknown type "carrier-pigeon"`,
		},
		{
			name:    "hass target without a service",
			mutate:  func(s string) string { return strings.Replace(s, "service: mobile_app_phone", "service: ''", 1) },
			wantMsg: "service is required for a hass target",
		},
		{
			name: "slack target without a bot token",
			mutate: func(s string) string {
				return strings.Replace(s, "type: hass, service: mobile_app_phone", "type: slack, channel: C0123", 1)
			},
			wantMsg: "slack.botToken is required",
		},
		{
			name: "slack target without a channel",
			mutate: func(s string) string {
				s = strings.Replace(s, "type: hass, service: mobile_app_phone", "type: slack", 1)
				return "slack: { botToken: xoxb-x }\n" + s
			},
			wantMsg: "channel is required for a slack target",
		},
		{
			name: "ntfy target without a server",
			mutate: func(s string) string {
				return strings.Replace(s, "type: hass, service: mobile_app_phone", "type: ntfy, topic: frigate", 1)
			},
			wantMsg: "ntfy.url is required",
		},
		{
			name: "discord webhook must be https",
			mutate: func(s string) string {
				return strings.Replace(s, "type: hass, service: mobile_app_phone", "type: discord, webhookURL: http://discord.test/x", 1)
			},
			wantMsg: "want an https webhook URL",
		},
		{
			name: "relative dashboardURL",
			mutate: func(s string) string {
				return strings.Replace(s, "timezone:", "dashboardURL: /lovelace/frigate\ntimezone:", 1)
			},
			wantMsg: "dashboardURL: want an absolute http(s) URL",
		},
		{
			name:    "notify service with domain prefix",
			mutate:  func(s string) string { return strings.Replace(s, "mobile_app_phone", "notify.mobile_app_phone", 1) },
			wantMsg: "bare service suffix",
		},
		{
			name: "liveview without an entity",
			mutate: func(s string) string {
				s = strings.Replace(s, "preset: auto", "preset: liveview", 1)
				s = strings.Replace(s, "labels: [person]", "cameras: [garage]", 1)
				return strings.Replace(s, "front_porch: { friendlyName: Front Porch, liveViewEntity: camera.front_porch }",
					"front_porch: { friendlyName: Front Porch }\n  garage: { friendlyName: Garage }", 1)
			},
			wantMsg: "liveview needs cameras.garage.liveViewEntity",
		},
		{
			name: "empty unless entry",
			mutate: func(s string) string {
				return strings.Replace(s, "    cooldown: 30s", "    unless:\n      - {}\n    cooldown: 30s", 1)
			},
			wantMsg: "would suppress the rule entirely",
		},
		{
			name: "duplicate rule names",
			mutate: func(s string) string {
				return s + `
  - name: day
    when: { labels: [dog] }
    to: [alice]
`
			},
			wantMsg: "duplicate rule name",
		},
		{
			// A missing endpoint parses as midnight, so half a window would
			// silently mean "22:00 to 00:00" rather than failing.
			name: "half-specified window in a rule",
			mutate: func(s string) string {
				return strings.Replace(s, `hours: { from: "06:00", to: "22:00" }`, `hours: { from: "06:00" }`, 1)
			},
			wantMsg: "hours: both from and to are required",
		},
		{
			name: "half-specified recipient window",
			mutate: func(s string) string {
				return strings.Replace(s, "allowCritical: true }",
					`allowCritical: true, activeHours: { from: "09:00" } }`, 1)
			},
			wantMsg: "activeHours: both from and to are required",
		},
		{
			// A liveview rule with no camera list applies to every camera, so
			// every camera needs an entity — the case the check used to skip.
			name: "liveview without an entity, no camera list",
			mutate: func(s string) string {
				s = strings.Replace(s, "preset: auto", "preset: liveview", 1)
				return strings.Replace(s, "front_porch: { friendlyName: Front Porch, liveViewEntity: camera.front_porch }",
					"front_porch: { friendlyName: Front Porch }", 1)
			},
			wantMsg: "liveview needs cameras.front_porch.liveViewEntity",
		},
		{
			name:    "bad timezone",
			mutate:  func(s string) string { return strings.Replace(s, "America/New_York", "Mars/Olympus", 1) },
			wantMsg: "timezone",
		},
		{
			name: "unsupported db scheme",
			mutate: func(s string) string {
				return strings.Replace(s, "timezone:", "db: { url: 'mysql://u@h/d' }\ntimezone:", 1)
			},
			wantMsg: `db.url: unsupported scheme "mysql"`,
		},
		{
			name: "bad rule timezone",
			mutate: func(s string) string {
				return strings.Replace(s, "  - name: day\n", "  - name: day\n    timezone: Mars/Olympus\n", 1)
			},
			wantMsg: `rules[0].timezone "Mars/Olympus"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Read(writeConfig(t, tc.mutate(minimalConfig)))
			if err == nil {
				t.Fatal("expected a validation error")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error = %v\nwant it to mention %q", err, tc.wantMsg)
			}
		})
	}
}

// Rules that exclude household faces must wait for face recognition, which
// lands after the review opens. Without the holdoff the exclusion is a no-op.
func TestEffectiveHoldoff(t *testing.T) {
	explicit := Duration(2)
	tests := []struct {
		name string
		rule Rule
		want string
	}{
		{name: "no face conditions", rule: Rule{}, want: "0s"},
		{name: "excludes sub-labels", rule: Rule{When: RuleConditions{ExcludeSubLabels: []string{"alice"}}}, want: "5s"},
		{name: "requires sub-labels", rule: Rule{When: RuleConditions{SubLabels: []string{"alice"}}}, want: "5s"},
		{name: "matches person label", rule: Rule{When: RuleConditions{Labels: []string{"person"}}}, want: "5s"},
		{name: "matches non-person label only", rule: Rule{When: RuleConditions{Labels: []string{"dog"}}}, want: "0s"},
		{name: "explicit overrides the default", rule: Rule{Holdoff: &explicit, When: RuleConditions{ExcludeSubLabels: []string{"alice"}}}, want: "2ns"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rule.EffectiveHoldoff().String(); got != tc.want {
				t.Errorf("EffectiveHoldoff() = %s, want %s", got, tc.want)
			}
		})
	}
}

// The shipped example must stay loadable, since it doubles as the reference
// for the chart values and the JSON schema.
func TestExampleConfigIsValid(t *testing.T) {
	raw, err := os.ReadFile("../../config-example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Fill in the values that are intentionally blank in the example.
	body := strings.NewReplacer(
		"token: ''", "token: 'x'",
		"botToken: ''", "botToken: 'x'",
		"password: ''", "password: 'x'",
		"signingKey: ''", "signingKey: '"+strings.Repeat("ab", 16)+"'",
	).Replace(string(raw))

	cfg, err := Read(writeConfig(t, body))
	if err != nil {
		t.Fatalf("config-example.yaml is not valid: %v", err)
	}
	if len(cfg.Rules) == 0 || len(cfg.Recipients) == 0 {
		t.Error("example should define rules and recipients")
	}
	if !cfg.Media.Enabled() {
		t.Error("example should have media configured")
	}
}

func TestDBURLSchemes(t *testing.T) {
	for _, url := range []string{"postgres://u@h/d", "postgresql://u@h/d", "mongodb://h/d", "mongodb+srv://h/d"} {
		body := strings.Replace(minimalConfig, "timezone:", "db: { url: '"+url+"' }\ntimezone:", 1)
		cfg, err := Read(writeConfig(t, body))
		if err != nil {
			t.Errorf("%s: %v", url, err)
			continue
		}
		if cfg.DB.URL != url {
			t.Errorf("DB.URL = %q, want %q", cfg.DB.URL, url)
		}
	}
}

func TestStoreURLEnvOverridesYAML(t *testing.T) {
	t.Setenv("REDIS_URL", "redis://env:6379")
	t.Setenv("DB_URL", "postgres://env@h/d")
	body := strings.Replace(minimalConfig, "timezone:", "redis: { url: 'redis://yaml:6379' }\ndb: { url: 'mongodb://yaml/d' }\ntimezone:", 1)
	cfg, err := Read(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if cfg.Redis.URL != "redis://env:6379" || cfg.DB.URL != "postgres://env@h/d" {
		t.Errorf("env should override yaml, got redis=%q db=%q", cfg.Redis.URL, cfg.DB.URL)
	}
}

func TestEnvBeatsYAML(t *testing.T) {
	env := map[string]string{
		"HASS_URL": "http://supervisor/core", "HASS_TOKEN": "sv-token",
		"MQTT_BROKER": "tcp://mosq:1883", "MQTT_USERNAME": "addon", "MQTT_PASSWORD": "secret", "MQTT_TOPIC_PREFIX": "frigate2",
		"MEDIA_FRIGATE_URL": "http://frigate:5000", "MEDIA_PUBLIC_BASE_URL": "https://n.example.com",
		"MEDIA_SIGNING_KEY": "00112233445566778899aabbccddeeff",
		"SLACK_BOT_TOKEN":   "xoxb-env", "NTFY_URL": "https://ntfy.env", "NTFY_TOKEN": "tk_env",
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	cfg, err := Read(writeConfig(t, minimalConfig))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{
		"HASS_URL": cfg.Hass.URL, "HASS_TOKEN": cfg.Hass.Token,
		"MQTT_BROKER": cfg.MQTT.Broker, "MQTT_USERNAME": cfg.MQTT.Username, "MQTT_PASSWORD": cfg.MQTT.Password, "MQTT_TOPIC_PREFIX": cfg.MQTT.TopicPrefix,
		"MEDIA_FRIGATE_URL": cfg.Media.FrigateURL, "MEDIA_PUBLIC_BASE_URL": cfg.Media.PublicBaseURL, "MEDIA_SIGNING_KEY": cfg.Media.SigningKey,
		"SLACK_BOT_TOKEN": cfg.Slack.BotToken, "NTFY_URL": cfg.Ntfy.URL, "NTFY_TOKEN": cfg.Ntfy.Token,
	}
	for k, want := range env {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
}

func TestEnvSuppliesWhatYAMLOmits(t *testing.T) {
	for k, v := range map[string]string{"HASS_URL": "http://h", "HASS_TOKEN": "tok", "MQTT_BROKER": "tcp://b:1883", "MQTT_USERNAME": "u", "MQTT_PASSWORD": "p"} {
		t.Setenv(k, v)
	}
	body := strings.Replace(minimalConfig, "hass: { url: http://hass, token: t }\n", "", 1)
	body = strings.Replace(body, "mqtt: { broker: tcp://b:1883, username: u, password: p }\n", "", 1)
	cfg, err := Read(writeConfig(t, body))
	if err != nil || cfg.Hass.Token != "tok" || cfg.MQTT.Password != "p" {
		t.Errorf("err = %v, hass.token = %q, mqtt.password = %q", err, cfg.Hass.Token, cfg.MQTT.Password)
	}
}

// docker-compose's `- HASS_TOKEN=` sets it empty; that must not blank the yaml.
func TestEmptyEnvDoesNotBlankYAML(t *testing.T) {
	t.Setenv("HASS_TOKEN", "")
	if cfg, err := Read(writeConfig(t, minimalConfig)); err != nil || cfg.Hass.Token != "t" {
		t.Errorf("err = %v, hass.token = %q, want t", err, cfg.Hass.Token)
	}
}

func TestRequiredConnectionSettingStillEnforced(t *testing.T) {
	t.Setenv("HASS_TOKEN", "")
	body := strings.Replace(minimalConfig, "hass: { url: http://hass, token: t }", "hass: { url: http://hass }", 1)
	if _, err := Read(writeConfig(t, body)); err == nil || !strings.Contains(err.Error(), "HASS_TOKEN") {
		t.Fatalf("err = %v", err)
	}
}
