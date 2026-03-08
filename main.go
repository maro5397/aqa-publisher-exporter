package main

import (
	"bufio"
	"context"
	"flag"
	"log"
	"net/http"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	listenAddr  = flag.String("web.listen-address", ":9100", "Address to listen on for HTTP requests")
	serviceName = flag.String("service", "aqa_publisher", "Systemd service name to monitor")
)

var (
	voteSubmitFailedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "aqa_publisher_vote_submit_failed_total",
		Help: "Total number of failed vote submissions for a signer.",
	})

	allVotesFailedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "aqa_publisher_all_votes_failed_total",
		Help: "Total number of scheduled runs where all validator votes failed.",
	})

	serviceUp = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "aqa_publisher_service_up",
		Help: "Whether the aqa_publisher systemd service is active (1 = active, 0 = inactive/failed).",
	})
)

type logPattern struct {
	substring string
	counter   prometheus.Counter
}

func watchLogs(ctx context.Context, service string, patterns []logPattern) {
	for {
		cmd := exec.CommandContext(ctx,
			"journalctl", "-u", service, "--follow", "--no-pager", "-n", "0",
		)

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			log.Printf("[watchLogs] stdout pipe failed: %v — retrying in 5s", err)
			goto retry
		}

		if err := cmd.Start(); err != nil {
			log.Printf("[watchLogs] journalctl start failed: %v — retrying in 5s", err)
			goto retry
		}

		log.Printf("[watchLogs] tailing unit=%s", service)
		func() {
			scanner := bufio.NewScanner(stdout)
			for scanner.Scan() {
				line := scanner.Text()
				for _, p := range patterns {
					if strings.Contains(line, p.substring) {
						p.counter.Inc()
					}
				}
			}
		}()
		_ = cmd.Wait()

	retry:
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func pollServiceStatus(ctx context.Context, service string) {
	for {
		cmd := exec.Command("systemctl", "is-active", "--quiet", service)
		if err := cmd.Run(); err == nil {
			serviceUp.Set(1)
		} else {
			serviceUp.Set(0)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}

func main() {
	flag.Parse()

	patterns := []logPattern{
		{"Failed to submit vote for signer", voteSubmitFailedTotal},
		{"All validator votes failed", allVotesFailedTotal},
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go watchLogs(ctx, *serviceName, patterns)
	go pollServiceStatus(ctx, *serviceName)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	server := &http.Server{
		Addr:    *listenAddr,
		Handler: mux,
	}

	go func() {
		log.Printf("aqa-publisher-exporter listening on %s", *listenAddr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("Shutting down gracefully...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}

	log.Println("Exporter exited.")
}
