package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"text/template"
	"time"

	"github.com/blang/semver/v4"
	"github.com/fatih/color"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/google/go-github/v32/github"
	"github.com/phyber/negroni-gzip/gzip"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"go.flipt.io/flipt/config"
	"go.flipt.io/flipt/internal/info"
	"go.flipt.io/flipt/internal/telemetry"
	pb "go.flipt.io/flipt/rpc/flipt"
	"go.flipt.io/flipt/server"
	"go.flipt.io/flipt/server/cache"
	"go.flipt.io/flipt/server/cache/memory"
	"go.flipt.io/flipt/server/cache/redis"
	"go.flipt.io/flipt/storage"
	"go.flipt.io/flipt/storage/sql"
	"go.flipt.io/flipt/storage/sql/mysql"
	"go.flipt.io/flipt/storage/sql/postgres"
	"go.flipt.io/flipt/storage/sql/sqlite"
	"go.flipt.io/flipt/swagger"
	"go.flipt.io/flipt/ui"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection"
	"google.golang.org/protobuf/encoding/protojson"
	"gopkg.in/segmentio/analytics-go.v3"

	_ "github.com/golang-migrate/migrate/source/file"

	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_logrus "github.com/grpc-ecosystem/go-grpc-middleware/logging/logrus"
	grpc_recovery "github.com/grpc-ecosystem/go-grpc-middleware/recovery"
	grpc_ctxtags "github.com/grpc-ecosystem/go-grpc-middleware/tags"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	grpc_gateway "github.com/grpc-ecosystem/grpc-gateway/v2/runtime"

	goredis_cache "github.com/go-redis/cache/v8"
	goredis "github.com/go-redis/redis/v8"
	otgrpc "github.com/opentracing-contrib/go-grpc"
	"github.com/opentracing/opentracing-go"
	jaeger_config "github.com/uber/jaeger-client-go/config"
)

const devVersion = "dev"

var (
	l   = logrus.New()
	cfg *config.Config

	cfgPath      string
	forceMigrate bool

	version      = devVersion
	commit       string
	date         string
	goVersion    = runtime.Version()
	analyticsKey string
	banner       string
)

func main() {
	var (
		rootCmd = &cobra.Command{
			Use:     "flipt",
			Short:   "Flipt is a modern feature flag solution",
			Version: version,
			Run: func(cmd *cobra.Command, _ []string) {
				if err := run(cmd.Context()); err != nil {
					logrus.Error(err)
					logrus.Exit(1)
				}
			},
			CompletionOptions: cobra.CompletionOptions{
				DisableDefaultCmd: true,
			},
		}

		exportCmd = &cobra.Command{
			Use:   "export",
			Short: "Export flags/segments/rules to file/stdout",
			Run: func(cmd *cobra.Command, _ []string) {
				if err := runExport(cmd.Context()); err != nil {
					logrus.Error(err)
					logrus.Exit(1)
				}
			},
		}

		importCmd = &cobra.Command{
			Use:   "import",
			Short: "Import flags/segments/rules from file",
			Run: func(cmd *cobra.Command, args []string) {
				if err := runImport(cmd.Context(), args); err != nil {
					logrus.Error(err)
					logrus.Exit(1)
				}
			},
		}

		migrateCmd = &cobra.Command{
			Use:   "migrate",
			Short: "Run pending database migrations",
			Run: func(cmd *cobra.Command, _ []string) {
				migrator, err := sql.NewMigrator(*cfg, l)
				if err != nil {
					logrus.Error(err)
					logrus.Exit(1)
				}

				defer migrator.Close()

				if err := migrator.Run(true); err != nil {
					logrus.Error(err)
					logrus.Exit(1)
				}
			},
		}
	)

	var (
		t   = template.Must(template.New("banner").Parse(bannerTmpl))
		buf = new(bytes.Buffer)
	)

	if err := t.Execute(buf, &bannerOpts{
		Version:   version,
		Commit:    commit,
		Date:      date,
		GoVersion: goVersion,
	}); err != nil {
		l.Errorf("executing template: %v", err)
		logrus.Exit(1)
	}

	banner = buf.String()

	cobra.OnInitialize(func() {
		var err error

		// read in config
		cfg, err = config.Load(cfgPath)
		if err != nil {
			l.Error(err)
			logrus.Exit(1)
		}

		l.SetOutput(os.Stdout)

		// log to file if enabled
		if cfg.Log.File != "" {
			logFile, err := os.OpenFile(cfg.Log.File, os.O_CREATE|os.O_WRONLY, 0600)
			if err != nil {
				l.Errorf("opening log file: %s %v", cfg.Log.File, err)
				logrus.Exit(1)
			}

			l.SetOutput(logFile)
			logrus.RegisterExitHandler(func() {
				if logFile != nil {
					_ = logFile.Close()
				}
			})
		}

		// parse/set log level
		lvl, err := logrus.ParseLevel(cfg.Log.Level)
		if err != nil {
			l.Errorf("parsing log level: %s %v", cfg.Log.Level, err)
			logrus.Exit(1)
		}

		l.SetLevel(lvl)
	})

	rootCmd.SetVersionTemplate(banner)
	rootCmd.PersistentFlags().StringVar(&cfgPath, "config", "/etc/flipt/config/default.yml", "path to config file")
	rootCmd.Flags().BoolVar(&forceMigrate, "force-migrate", false, "force migrations before running")
	_ = rootCmd.Flags().MarkHidden("force-migrate")

	exportCmd.Flags().StringVarP(&exportFilename, "output", "o", "", "export to filename (default STDOUT)")
	importCmd.Flags().BoolVar(&dropBeforeImport, "drop", false, "drop database before import")
	importCmd.Flags().BoolVar(&importStdin, "stdin", false, "import from STDIN")

	rootCmd.AddCommand(migrateCmd)
	rootCmd.AddCommand(exportCmd)
	rootCmd.AddCommand(importCmd)

	if err := rootCmd.Execute(); err != nil {
		l.Error(err)
		logrus.Exit(1)
	}

	logrus.Exit(0)
}

func run(ctx context.Context) error {
	color.Cyan(banner)
	fmt.Println()

	ctx, cancel := context.WithCancel(ctx)

	defer cancel()

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)

	defer signal.Stop(interrupt)

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	var (
		isRelease       = isRelease()
		updateAvailable bool
		cv, lv          semver.Version
	)

	if isRelease {
		var err error
		cv, err = semver.ParseTolerant(version)
		if err != nil {
			return fmt.Errorf("parsing version: %w", err)
		}
	}

	// print out any warnings from config parsing
	for _, warning := range cfg.Warnings {
		l.Warn(warning)
	}

	if cfg.Meta.CheckForUpdates && isRelease {
		l.Debug("checking for updates...")

		release, err := getLatestRelease(ctx)
		if err != nil {
			l.Warn(err)
		}

		if release != nil {
			var err error
			lv, err = semver.ParseTolerant(release.GetTagName())
			if err != nil {
				return fmt.Errorf("parsing latest version: %w", err)
			}

			l.Debugf("current version: %s; latest version: %s", cv, lv)

			switch cv.Compare(lv) {
			case 0:
				color.Green("You are currently running the latest version of Flipt [%s]!", cv)
			case -1:
				updateAvailable = true
				color.Yellow("A newer version of Flipt exists at %s, \nplease consider updating to the latest version.", release.GetHTMLURL())
			}
		}
	}

	info := info.Flipt{
		Commit:          commit,
		BuildDate:       date,
		GoVersion:       goVersion,
		Version:         cv.String(),
		LatestVersion:   lv.String(),
		IsRelease:       isRelease,
		UpdateAvailable: updateAvailable,
	}

	if os.Getenv("CI") == "true" || os.Getenv("CI") == "1" {
		l.Debug("CI detected, disabling telemetry")
		cfg.Meta.TelemetryEnabled = false
	}

	g, ctx := errgroup.WithContext(ctx)

	if cfg.Meta.TelemetryEnabled && isRelease {
		if err := initLocalState(); err != nil {
			l.Warnf("error getting local state directory: %s, disabling telemetry: %s", cfg.Meta.StateDirectory, err)
			cfg.Meta.TelemetryEnabled = false
		} else {
			l.Debugf("local state directory exists: %s", cfg.Meta.StateDirectory)
		}

		var (
			reportInterval = 4 * time.Hour
			ticker         = time.NewTicker(reportInterval)
		)

		defer ticker.Stop()

		// start telemetry if enabled
		g.Go(func() error {
			logger := l.WithField("component", "telemetry")

			// don't log from analytics package
			analyticsLogger := func() analytics.Logger {
				stdLogger := log.Default()
				stdLogger.SetOutput(ioutil.Discard)
				return analytics.StdLogger(stdLogger)
			}

			client, err := analytics.NewWithConfig(analyticsKey, analytics.Config{
				BatchSize: 1,
				Logger:    analyticsLogger(),
			})
			if err != nil {
				logger.Warnf("error initializing telemetry client: %s", err)
				return nil
			}

			telemetry := telemetry.NewReporter(*cfg, logger, client)
			defer telemetry.Close()

			logger.Debug("starting telemetry reporter")
			if err := telemetry.Report(ctx, info); err != nil {
				logger.Warnf("reporting telemetry: %v", err)
			}

			for {
				select {
				case <-ticker.C:
					if err := telemetry.Report(ctx, info); err != nil {
						logger.Warnf("reporting telemetry: %v", err)
					}
				case <-ctx.Done():
					ticker.Stop()
					return nil
				}
			}
		})
	}

	var (
		grpcServer *grpc.Server
		httpServer *http.Server
	)

	// starts grpc server
	g.Go(func() error {
		logger := l.WithField("server", "grpc")

		migrator, err := sql.NewMigrator(*cfg, l)
		if err != nil {
			return err
		}

		defer migrator.Close()

		if err := migrator.Run(forceMigrate); err != nil {
			return err
		}

		migrator.Close()

		lis, err := net.Listen("tcp", fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.GRPCPort))
		if err != nil {
			return fmt.Errorf("creating grpc listener: %w", err)
		}

		defer func() {
			_ = lis.Close()
		}()

		db, driver, err := sql.Open(*cfg)
		if err != nil {
			return fmt.Errorf("opening db: %w", err)
		}

		defer db.Close()

		if err := db.PingContext(ctx); err != nil {
			return fmt.Errorf("pinging db: %w", err)
		}

		var store storage.Store

		switch driver {
		case sql.SQLite:
			store = sqlite.NewStore(db)
		case sql.Postgres:
			store = postgres.NewStore(db)
		case sql.MySQL:
			store = mysql.NewStore(db)
		}

		logger.Debugf("store: %q enabled", store.String())

		var tracer opentracing.Tracer = &opentracing.NoopTracer{}

		if cfg.Tracing.Jaeger.Enabled {
			jaegerCfg := jaeger_config.Configuration{
				ServiceName: "flipt",
				Sampler: &jaeger_config.SamplerConfig{
					Type:  "const",
					Param: 1,
				},
				Reporter: &jaeger_config.ReporterConfig{
					LocalAgentHostPort:  fmt.Sprintf("%s:%d", cfg.Tracing.Jaeger.Host, cfg.Tracing.Jaeger.Port),
					LogSpans:            true,
					BufferFlushInterval: 1 * time.Second,
				},
			}

			var closer io.Closer

			tracer, closer, err = jaegerCfg.NewTracer(jaeger_config.Logger(&jaegerLogAdapter{logger}))
			if err != nil {
				return fmt.Errorf("configuring tracing: %w", err)
			}

			defer closer.Close()
		}

		opentracing.SetGlobalTracer(tracer)

		interceptors := []grpc.UnaryServerInterceptor{
			grpc_recovery.UnaryServerInterceptor(),
			grpc_ctxtags.UnaryServerInterceptor(),
			grpc_logrus.UnaryServerInterceptor(logger),
			grpc_prometheus.UnaryServerInterceptor,
			otgrpc.OpenTracingServerInterceptor(tracer),
			server.ErrorUnaryInterceptor,
			server.ValidationUnaryInterceptor,
			server.EvaluationUnaryInterceptor,
		}

		if cfg.Cache.Enabled {
			var cacher cache.Cacher

			switch cfg.Cache.Backend {
			case config.CacheMemory:
				cacher = memory.NewCache(cfg.Cache)
			case config.CacheRedis:
				rdb := goredis.NewClient(&goredis.Options{
					Addr:     fmt.Sprintf("%s:%d", cfg.Cache.Redis.Host, cfg.Cache.Redis.Port),
					Password: cfg.Cache.Redis.Password,
					DB:       cfg.Cache.Redis.DB,
				})

				defer rdb.Shutdown(shutdownCtx)

				status := rdb.Ping(ctx)
				if status == nil {
					return errors.New("connecting to redis: no status")
				}

				if status.Err() != nil {
					return fmt.Errorf("connecting to redis: %w", status.Err())
				}

				cacher = redis.NewCache(cfg.Cache, goredis_cache.New(&goredis_cache.Options{
					Redis: rdb,
				}))
			}

			interceptors = append(interceptors, server.CacheUnaryInterceptor(cacher, logger))

			logger.Debugf("cache: %q enabled", cacher.String())
		}

		grpcOpts := []grpc.ServerOption{grpc_middleware.WithUnaryServerChain(interceptors...)}

		if cfg.Server.Protocol == config.HTTPS {
			creds, err := credentials.NewServerTLSFromFile(cfg.Server.CertFile, cfg.Server.CertKey)
			if err != nil {
				return fmt.Errorf("loading TLS credentials: %w", err)
			}

			grpcOpts = append(grpcOpts, grpc.Creds(creds))
		}

		// initialize server
		srv := server.New(logger, store)
		// initialize grpc server
		grpcServer = grpc.NewServer(grpcOpts...)

		pb.RegisterFliptServer(grpcServer, srv)
		grpc_prometheus.EnableHandlingTimeHistogram()
		grpc_prometheus.Register(grpcServer)
		reflection.Register(grpcServer)

		logger.Debug("starting grpc server")
		return grpcServer.Serve(lis)
	})

	// starts REST http(s) server
	g.Go(func() error {
		logger := l.WithField("server", cfg.Server.Protocol.String())

		var (
			// This is required to fix a backwards compatibility issue with the v2 marshaller where `null` map values
			// cause an error because they are not allowed by the proto spec, but they were handled by the v1 marshaller.
			//
			// See: rpc/flipt/marshal.go
			//
			// See: https://github.com/flipt-io/flipt/issues/664
			muxOpts = []grpc_gateway.ServeMuxOption{
				grpc_gateway.WithMarshalerOption(grpc_gateway.MIMEWildcard, pb.NewV1toV2MarshallerAdapter()),
				grpc_gateway.WithMarshalerOption("application/json+pretty", &grpc_gateway.JSONPb{
					MarshalOptions: protojson.MarshalOptions{
						Indent:    "  ",
						Multiline: true, // Optional, implied by presence of "Indent".
					},
					UnmarshalOptions: protojson.UnmarshalOptions{
						DiscardUnknown: true,
					},
				}),
			}

			r        = chi.NewRouter()
			api      = grpc_gateway.NewServeMux(muxOpts...)
			opts     = []grpc.DialOption{grpc.WithBlock()}
			httpPort int
		)

		switch cfg.Server.Protocol {
		case config.HTTPS:
			creds, err := credentials.NewClientTLSFromFile(cfg.Server.CertFile, "")
			if err != nil {
				return fmt.Errorf("loading TLS credentials: %w", err)
			}

			opts = append(opts, grpc.WithTransportCredentials(creds))
			httpPort = cfg.Server.HTTPSPort
		case config.HTTP:
			opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
			httpPort = cfg.Server.HTTPPort
		}

		dialCtx, dialCancel := context.WithTimeout(ctx, 5*time.Second)
		defer dialCancel()

		conn, err := grpc.DialContext(dialCtx, fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.GRPCPort), opts...)
		if err != nil {
			return fmt.Errorf("connecting to grpc server: %w", err)
		}

		if err := pb.RegisterFliptHandler(ctx, api, conn); err != nil {
			return fmt.Errorf("registering grpc gateway: %w", err)
		}

		if cfg.Cors.Enabled {
			cors := cors.New(cors.Options{
				AllowedOrigins:   cfg.Cors.AllowedOrigins,
				AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
				AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"},
				ExposedHeaders:   []string{"Link"},
				AllowCredentials: true,
				MaxAge:           300,
			})

			r.Use(cors.Handler)
			logger.Infof("CORS enabled with allowed origins: %v", cfg.Cors.AllowedOrigins)
		}

		r.Use(middleware.RequestID)
		r.Use(middleware.RealIP)
		r.Use(middleware.Heartbeat("/health"))
		r.Use(func(h http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// checking Values as map[string][]string also catches ?pretty and ?pretty=
				// r.URL.Query().Get("pretty") would not.
				if _, ok := r.URL.Query()["pretty"]; ok {
					r.Header.Set("Accept", "application/json+pretty")
				}
				h.ServeHTTP(w, r)
			})
		})
		r.Use(middleware.Compress(gzip.DefaultCompression))
		r.Use(middleware.Recoverer)
		r.Mount("/metrics", promhttp.Handler())
		r.Mount("/api/v1", api)
		r.Mount("/debug", middleware.Profiler())

		r.Route("/meta", func(r chi.Router) {
			r.Use(middleware.SetHeader("Content-Type", "application/json"))
			r.Handle("/info", info)
			r.Handle("/config", cfg)
		})

		if cfg.UI.Enabled {
			s := http.FS(swagger.Docs)
			r.Mount("/docs", http.StripPrefix("/docs/", http.FileServer(s)))

			u, err := fs.Sub(ui.UI, "dist")
			if err != nil {
				return fmt.Errorf("mounting UI: %w", err)
			}

			r.Mount("/", http.FileServer(http.FS(u)))
		}

		httpServer = &http.Server{
			Addr:           fmt.Sprintf("%s:%d", cfg.Server.Host, httpPort),
			Handler:        r,
			ReadTimeout:    10 * time.Second,
			WriteTimeout:   30 * time.Second,
			MaxHeaderBytes: 1 << 20,
		}

		logger.Debug("starting http server")

		color.Green("\nAPI: %s://%s:%d/api/v1", cfg.Server.Protocol, cfg.Server.Host, httpPort)

		if cfg.UI.Enabled {
			color.Green("UI: %s://%s:%d", cfg.Server.Protocol, cfg.Server.Host, httpPort)
		}

		fmt.Println()

		if cfg.Server.Protocol == config.HTTPS {
			httpServer.TLSConfig = &tls.Config{
				MinVersion:               tls.VersionTLS12,
				PreferServerCipherSuites: true,
				CipherSuites: []uint16{
					tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
					tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
					tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
					tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
				},
			}

			httpServer.TLSNextProto = make(map[string]func(*http.Server, *tls.Conn, http.Handler))

			err = httpServer.ListenAndServeTLS(cfg.Server.CertFile, cfg.Server.CertKey)
		} else {
			err = httpServer.ListenAndServe()
		}

		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}

		logger.Info("server shutdown gracefully")
		return nil
	})

	select {
	case <-interrupt:
		break
	case <-ctx.Done():
		break
	}

	l.Info("shutting down...")

	cancel()

	if httpServer != nil {
		_ = httpServer.Shutdown(shutdownCtx)
	}

	if grpcServer != nil {
		grpcServer.GracefulStop()
	}

	return g.Wait()
}

func getLatestRelease(ctx context.Context) (*github.RepositoryRelease, error) {
	client := github.NewClient(nil)
	release, _, err := client.Repositories.GetLatestRelease(ctx, "flipt-io", "flipt")
	if err != nil {
		return nil, fmt.Errorf("checking for latest version: %w", err)
	}

	return release, nil
}

func isRelease() bool {
	if version == "" || version == devVersion {
		return false
	}
	if strings.HasSuffix(version, "-snapshot") {
		return false
	}
	return true
}

// check if state directory already exists, create it if not
func initLocalState() error {
	if cfg.Meta.StateDirectory == "" {
		configDir, err := os.UserConfigDir()
		if err != nil {
			return fmt.Errorf("getting user config dir: %w", err)
		}
		cfg.Meta.StateDirectory = filepath.Join(configDir, "flipt")
	}

	fp, err := os.Stat(cfg.Meta.StateDirectory)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// state directory doesnt exist, so try to create it
			return os.MkdirAll(cfg.Meta.StateDirectory, 0700)
		}
		return fmt.Errorf("checking state directory: %w", err)
	}

	if fp != nil && !fp.IsDir() {
		return fmt.Errorf("state directory is not a directory")
	}

	// assume state directory exists and is a directory
	return nil
}

// jaegerLogAdapter adapts logrus to fulfill Jager's Logger interface
type jaegerLogAdapter struct {
	*logrus.Entry
}

// Error logs a message at error priority
func (l *jaegerLogAdapter) Error(msg string) {
	l.Entry.Error(msg)
}
