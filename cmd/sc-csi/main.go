// Command sc-csi is a CSI driver for Dell SC Series (Compellent) arrays over
// Fibre Channel.
//
//	sc-csi controller   serve the CSI Identity and Controller services
//	sc-csi node         serve the CSI Identity and Node services
//	sc-csi audit        cross-check the driver's array folders against the cluster
//	sc-csi version      print the version
//
// Settings come from flags or environment variables; see usage below.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/charlie-horse/sc-csi/internal/csiserver"
	"github.com/charlie-horse/sc-csi/internal/csp"
	"github.com/charlie-horse/sc-csi/internal/driver"
	"github.com/charlie-horse/sc-csi/internal/monitor"
	"github.com/charlie-horse/sc-csi/internal/sc"
)

var version = "dev"

const usage = `usage: sc-csi controller|node|audit|version [flags]

Environment (each X_FILE variable names a file to read X from, e.g. a mounted Secret):
  SC_HOST[_FILE], SC_PORT (3033), SC_USER[_FILE], SC_PASSWORD[_FILE], SC_SSN
  SC_TLS_FINGERPRINT[_FILE] (SHA-256 of the array certificate), SC_CA_FILE, SC_TLS_INSECURE
  SC_VOLUME_FOLDER, SC_SERVER_FOLDER, SC_SERVER_OS, SC_STORAGE_PROFILE, SC_DATA_REDUCTION,
  SC_SNAPSHOT_PROFILE, LOG_LEVEL (debug|info|warn|error)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	log := newLogger()
	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "controller":
		err = runController(log, args)
	case "node":
		err = runNode(log, args)
	case "audit":
		err = audit(log, args)
	case "version":
		fmt.Println(version)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		log.Error("exit", "err", err)
		os.Exit(1)
	}
}

func newLogger() *slog.Logger {
	lvl := slog.LevelInfo
	_ = lvl.UnmarshalText([]byte(env("LOG_LEVEL", "info")))
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envBool(k string, def bool) bool {
	v, err := strconv.ParseBool(env(k, strconv.FormatBool(def)))
	if err != nil {
		return def
	}
	return v
}

// secret returns the value of k, or the trimmed contents of the file named
// by k_FILE when that is set.
func secret(k string) (string, error) {
	if f := os.Getenv(k + "_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return "", fmt.Errorf("%s_FILE: %w", k, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return os.Getenv(k), nil
}

func serviceConfig() csp.Config {
	d := csp.Defaults()
	return csp.Config{
		VolumeFolder:    env("SC_VOLUME_FOLDER", d.VolumeFolder),
		ServerFolder:    env("SC_SERVER_FOLDER", d.ServerFolder),
		ServerOS:        env("SC_SERVER_OS", d.ServerOS),
		StorageProfile:  env("SC_STORAGE_PROFILE", d.StorageProfile),
		DataReduction:   env("SC_DATA_REDUCTION", d.DataReduction),
		SnapshotProfile: os.Getenv("SC_SNAPSHOT_PROFILE"),
	}
}

// arrayConfig reads the array address, credentials and TLS trust settings.
func arrayConfig() (sc.Config, error) {
	var cfg sc.Config
	var err error
	for _, f := range []struct {
		key string
		dst *string
	}{{"SC_HOST", &cfg.Host}, {"SC_USER", &cfg.User}, {"SC_PASSWORD", &cfg.Password}, {"SC_TLS_FINGERPRINT", &cfg.Fingerprint}} {
		if *f.dst, err = secret(f.key); err != nil {
			return cfg, err
		}
	}
	if cfg.Host == "" || cfg.User == "" || cfg.Password == "" {
		return cfg, errors.New("set SC_HOST, SC_USER and SC_PASSWORD (or their _FILE variants)")
	}
	cfg.Port = env("SC_PORT", "3033")
	cfg.SSN = os.Getenv("SC_SSN")
	cfg.CAFile = os.Getenv("SC_CA_FILE")
	cfg.InsecureSkipVerify = envBool("SC_TLS_INSECURE", false)
	return cfg, nil
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}

func runController(log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("controller", flag.ExitOnError)
	endpoint := fs.String("endpoint", env("CSI_ENDPOINT", "unix:///csi/csi.sock"), "CSI endpoint (env CSI_ENDPOINT)")
	metricsAddr := fs.String("metrics-listen", env("METRICS_LISTEN", ":9090"), "Prometheus /metrics address; empty disables (env METRICS_LISTEN)")
	interval := fs.Duration("monitor-interval", 60*time.Second, "array health poll interval; 0 disables")
	_ = fs.Parse(args)

	ac, err := arrayConfig()
	if err != nil {
		return err
	}
	if ac.Fingerprint == "" && ac.CAFile == "" && !ac.InsecureSkipVerify {
		return errors.New("no TLS trust configured: set SC_TLS_FINGERPRINT, SC_CA_FILE or SC_TLS_INSECURE=true")
	}
	ctx, stop := signalContext()
	defer stop()

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	am := newArrayMetrics(reg)

	array, err := sc.New(ctx, ac, log, am.observe)
	if err != nil {
		return fmt.Errorf("array login: %w", err)
	}
	defer array.Logout(context.Background())

	cfg := serviceConfig()
	d := &driver.Driver{
		Cfg:   driver.Config{Version: version},
		Log:   log,
		CSP:   &csp.Service{Cfg: cfg, Log: log},
		Array: array,
	}
	if *interval > 0 {
		health := monitor.NewMetrics(reg)
		go health.Run(ctx, log, *interval, func(context.Context) (*sc.Client, error) { return array, nil },
			func() bool { return true })
	}
	if *metricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
		met := &http.Server{Addr: *metricsAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := met.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("metrics server", "err", err)
			}
		}()
		defer met.Close()
	}

	srv := csiserver.NewServer(log)
	csi.RegisterIdentityServer(srv, d)
	csi.RegisterControllerServer(srv, d)
	log.Info("sc-csi controller started", "version", version, "endpoint", *endpoint, "array", ac.Host,
		"ssn", array.SSN(), "config", cfg, "metrics", *metricsAddr)
	return csiserver.Serve(ctx, *endpoint, srv)
}

func runNode(log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("node", flag.ExitOnError)
	endpoint := fs.String("endpoint", env("CSI_ENDPOINT", "unix:///csi/csi.sock"), "CSI endpoint (env CSI_ENDPOINT)")
	nodeName := fs.String("node-name", os.Getenv("NODE_NAME"), "Kubernetes node name (env NODE_NAME)")
	hostRoot := fs.String("host-root", os.Getenv("HOST_ROOT"), "host / mount point; the multipath tools run chrooted into it (env HOST_ROOT)")
	hostPath := fs.String("host-path", os.Getenv("HOST_PATH"), "directories put in front of PATH, colon separated, e.g. /run/current-system/sw/bin (env HOST_PATH)")
	_ = fs.Parse(args)
	if *nodeName == "" {
		return errors.New("--node-name or NODE_NAME is required")
	}
	ctx, stop := signalContext()
	defer stop()

	d, err := nodeDriver(log, *nodeName, *hostRoot, *hostPath)
	if err != nil {
		return err
	}
	srv := csiserver.NewServer(log)
	csi.RegisterIdentityServer(srv, d)
	csi.RegisterNodeServer(srv, d)
	log.Info("sc-csi node started", "version", version, "endpoint", *endpoint, "node", *nodeName)
	return csiserver.Serve(ctx, *endpoint, srv)
}
