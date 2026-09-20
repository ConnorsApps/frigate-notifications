package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ConnorsApps/frigate-notifications/internal/config"
	"github.com/ConnorsApps/frigate-notifications/internal/db"
	"github.com/ConnorsApps/frigate-notifications/internal/frigate"
	"github.com/ConnorsApps/frigate-notifications/internal/hasscache"
	"github.com/ConnorsApps/frigate-notifications/internal/lib/hass"
	"github.com/ConnorsApps/frigate-notifications/internal/lib/httpserver"
	homelog "github.com/ConnorsApps/frigate-notifications/internal/lib/log"
	home_otel "github.com/ConnorsApps/frigate-notifications/internal/lib/otel"
	"github.com/ConnorsApps/frigate-notifications/internal/media"
	"github.com/ConnorsApps/frigate-notifications/internal/metrics"
	"github.com/ConnorsApps/frigate-notifications/internal/notifier"
	"github.com/ConnorsApps/frigate-notifications/internal/sender"
	"github.com/ConnorsApps/frigate-notifications/internal/store"
	"github.com/caarlos0/env/v11"
	"github.com/rs/zerolog/log"
)

const serviceName = "FrigateNotify"

const (
	// internalAddr serves health and metrics. It is never routed through the
	// public gateway.
	internalAddr = ":8080"
	// mediaAddr serves signed media links and nothing else, because the
	// networking chart has no path matching: whatever port is routed
	// publicly exposes every handler on it.
	mediaAddr = ":8081"
)

type envConfig struct {
	ConfigPath string `env:"CONFIG_PATH" envDefault:"config.yaml"`
	Version    string `env:"VERSION"`
}

// ready mirrors the Frigate MQTT connection state; exposed via /readyz.
var ready atomic.Bool

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var ec envConfig
	if err := env.Parse(&ec); err != nil {
		log.Fatal().Err(err).Msg("failed to parse env config")
	}

	homelog.Setup()

	cfg := config.MustRead(ec.ConfigPath)
	homelog.Setup(homelog.WithLevelStr(cfg.LogLevel))

	shutdown, err := home_otel.Setup(ctx, serviceName, ec.Version)
	if err != nil {
		log.Warn().Err(err).Msg("failed to setup telemetry")
	} else {
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := shutdown(shutdownCtx); err != nil {
				log.Warn().Err(err).Msg("telemetry shutdown error")
			}
		}()
	}

	m := metrics.New()

	hassClient := hass.New(cfg.Hass.URL, cfg.Hass.Token)
	states := hasscache.New(hassClient, cfg.Location)

	var database db.DB
	if cfg.DB.URL != "" {
		database, err = db.Open(ctx, cfg.DB.URL, m)
		if err != nil {
			log.Warn().Err(err).Msg("failed to connect to db; persistence disabled")
		} else {
			defer func() {
				closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				database.Close(closeCtx)
			}()
		}
	} else {
		log.Warn().Msg("db is not configured; review/notification events will not be persisted")
	}

	var storeOpts []store.Option
	if database != nil {
		storeOpts = append(storeOpts, store.WithBackend(database))
	}
	st := store.New(ctx, cfg.Redis.URL, m, storeOpts...)
	defer st.Close()

	signer, err := media.SignerFor(cfg.Media)
	if err != nil {
		log.Fatal().Err(err).Msg("invalid media configuration")
	}
	if signer == nil {
		log.Warn().Msg("media is not configured; notifications will be text-only")
	}

	notifierOpts := []notifier.Option{notifier.WithMetrics(m)}
	if signer != nil {
		notifierOpts = append(notifierOpts, notifier.WithMediaProber(media.NewProber(cfg.Media.FrigateURL)))
	}
	if database != nil {
		notifierOpts = append(notifierOpts, notifier.WithEventStore(database))
	}
	senders := sender.New(cfg, hassClient)
	n := notifier.New(cfg, senders, states, st, signer, notifierOpts...)

	go verifyTargets(ctx, senders, cfg)

	httpErrCh := make(chan error, 2)

	go func() {
		httpErrCh <- httpserver.RunGraceful(ctx, internalAddr, internalMux())
	}()

	if signer != nil {
		proxy := media.NewProxy(signer, cfg.Media.FrigateURL, media.WithMetrics(m))
		go func() {
			httpErrCh <- httpserver.RunGraceful(ctx, mediaAddr, proxy.Handler())
		}()
	}

	log.Info().Str("internal", internalAddr).Str("media", mediaAddr).Msg("listening")

	sub, err := frigate.NewSubscriber(ctx, frigate.Options{
		Broker:      cfg.MQTT.Broker,
		ClientID:    cfg.MQTT.ClientID,
		Username:    cfg.MQTT.Username,
		Password:    cfg.MQTT.Password,
		TopicPrefix: cfg.MQTT.TopicPrefix,
		OnReady:     ready.Store,
	}, frigate.Handlers{
		Review: func(event frigate.ReviewEvent) {
			n.HandleReview(ctx, event)
		},
		TrackedObjectUpdate: func(upd frigate.TrackedObjectUpdate) {
			n.HandleTrackedObjectUpdate(ctx, upd)
		},
	})
	if err != nil {
		log.Fatal().Err(err).Msg("failed to connect to mqtt broker")
	}
	defer sub.Close()

	if err := <-httpErrCh; err != nil {
		log.Fatal().Err(err).Msg("http server error")
	}
}

func internalMux() http.Handler {
	mux := http.NewServeMux()
	// Liveness: never tied to MQTT state, so a slow broker connect can't
	// get the pod SIGKILLed before it logs why.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

// verifyTargets warns about targets a backend says it can't deliver to. A
// typo'd service name or a revoked token otherwise fails silently on every
// notification, forever, with nothing to notice until you need it.
func verifyTargets(ctx context.Context, senders sender.Registry, cfg *config.Config) {
	for _, err := range senders.Verify(ctx, cfg.Recipients) {
		log.Error().Err(err).Msg("could not verify notification targets")
	}
}
