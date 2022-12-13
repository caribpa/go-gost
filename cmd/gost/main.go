package main

import (
	"flag"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"os/exec"
	"strings"
	"sync"
	"context"
	"runtime"
	"syscall"

	"github.com/go-gost/core/logger"
	"github.com/go-gost/x/config"
	"github.com/go-gost/x/config/parsing"
	xlogger "github.com/go-gost/x/logger"
	xmetrics "github.com/go-gost/x/metrics"
	"github.com/go-gost/x/registry"
)

var (
	log logger.Logger

	cfgFile      string
	outputFormat string
	services     stringList
	nodes        stringList
	debug        bool
	apiAddr      string
	metricsAddr  string
)

func init() {
	args := strings.Join(os.Args[1:], "  ")

	if strings.Contains(args, " -- ") {
		var (
			wg sync.WaitGroup
			ret int
		)

		ctx, cancel := context.WithCancel(context.Background())

		for wid, wargs := range strings.Split(" " + args + " ", " -- ") {
			wg.Add(1)
			go func(wid int, wargs string) {
				defer wg.Done()
				defer cancel()
				worker(wid, strings.Split(wargs, "  "), &ctx, &ret)
			}(wid, strings.TrimSpace(wargs))
		}

		wg.Wait()

		os.Exit(ret)
	}
}

func worker(id int, args []string, ctx *context.Context, ret *int) {
	cmd := exec.CommandContext(*ctx, os.Args[0], args...)

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), fmt.Sprintf("_GOST_ID=%d", id))

	cmd.Run()
	if cmd.ProcessState.Exited() {
		*ret = cmd.ProcessState.ExitCode()
	}
}

func init() {
	var printVersion bool

	flag.Var(&services, "L", "service list")
	flag.Var(&nodes, "F", "chain node list")
	flag.StringVar(&cfgFile, "C", "", "configuration file")
	flag.BoolVar(&printVersion, "V", false, "print version")
	flag.StringVar(&outputFormat, "O", "", "output format, one of yaml|json format")
	flag.BoolVar(&debug, "D", false, "debug mode")
	flag.StringVar(&apiAddr, "api", "", "api service address")
	flag.StringVar(&metricsAddr, "metrics", "", "metrics service address")
	flag.Parse()

	if printVersion {
		fmt.Fprintf(os.Stdout, "gost %s (%s %s/%s)\n",
			version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		os.Exit(0)
	}

	log = xlogger.NewLogger()
	logger.SetDefault(log)
}

func main() {
	cfg := &config.Config{}
	if cfgFile != "" {
		if err := cfg.ReadFile(cfgFile); err != nil {
			log.Fatal(err)
		}
	}

	cmdCfg, err := buildConfigFromCmd(services, nodes)
	if err != nil {
		log.Fatal(err)
	}
	cfg.Chains = append(cfg.Chains, cmdCfg.Chains...)
	cfg.Services = append(cfg.Services, cmdCfg.Services...)

	if len(cfg.Services) == 0 && apiAddr == "" {
		if err := cfg.Load(); err != nil {
			log.Fatal(err)
		}
	}

	if debug {
		if cfg.Log == nil {
			cfg.Log = &config.LogConfig{}
		}
		cfg.Log.Level = string(logger.DebugLevel)
	}
	if apiAddr != "" {
		cfg.API = &config.APIConfig{
			Addr: apiAddr,
		}
	}
	if metricsAddr != "" {
		cfg.Metrics = &config.MetricsConfig{
			Addr: metricsAddr,
		}
	}

	log = logFromConfig(cfg.Log)

	logger.SetDefault(log)

	if outputFormat != "" {
		if err := cfg.Write(os.Stdout, outputFormat); err != nil {
			log.Fatal(err)
		}
		os.Exit(0)
	}

	if cfg.Profiling != nil {
		go func() {
			addr := cfg.Profiling.Addr
			if addr == "" {
				addr = ":6060"
			}
			log.Info("profiling server on ", addr)
			log.Fatal(http.ListenAndServe(addr, nil))
		}()
	}

	if cfg.API != nil {
		s, err := buildAPIService(cfg.API)
		if err != nil {
			log.Fatal(err)
		}
		defer s.Close()

		go func() {
			log.Info("api service on ", s.Addr())
			log.Fatal(s.Serve())
		}()
	}

	if cfg.Metrics != nil {
		xmetrics.Init(xmetrics.NewMetrics())
		if cfg.Metrics.Addr != "" {
			s, err := buildMetricsService(cfg.Metrics)
			if err != nil {
				log.Fatal(err)
			}
			go func() {
				defer s.Close()
				log.Info("metrics service on ", s.Addr())
				log.Fatal(s.Serve())
			}()
		}
	}

	parsing.BuildDefaultTLSConfig(cfg.TLS)

	services := buildService(cfg)
	for _, svc := range services {
		svc := svc
		go func() {
			svc.Serve()
			// svc.Close()
		}()
	}

	config.SetGlobal(cfg)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	for sig := range sigs {
		switch sig {
		case syscall.SIGHUP:
			return
		default:
			for name, srv := range registry.ServiceRegistry().GetAll() {
				srv.Close()
				log.Debugf("service %s shutdown", name)
			}
			return
		}
	}
}
