package main

import (
	"bufio"
	"flag"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	listenAddr  = flag.String("web.listen-address", ":9101", "Address to listen on for HTTP requests")
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

// watchLogs tails the systemd journal for the service and increments counters
// on matching log lines. It automatically restarts if journalctl exits.
func watchLogs(service string) {
	for {
		cmd := exec.Command(
			"journalctl",
			"-u", service,
			"--follow",
			"--no-pager",
			"-n", "0", // skip historical entries; only stream new lines
		)

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			log.Printf("[watchLogs] failed to create stdout pipe: %v — retrying in 5s", err)
			time.Sleep(5 * time.Second)
			continue
		}

		if err := cmd.Start(); err != nil {
			log.Printf("[watchLogs] failed to start journalctl: %v — retrying in 5s", err)
			time.Sleep(5 * time.Second)
			continue
		}

		log.Printf("[watchLogs] started tailing journalctl for unit=%s", service)

		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "Failed to submit vote for signer") {
				voteSubmitFailedTotal.Inc()
				log.Printf("[watchLogs] vote_submit_failed detected: %s", line)
			}
			if strings.Contains(line, "All validator votes failed") {
				allVotesFailedTotal.Inc()
				log.Printf("[watchLogs] all_votes_failed detected: %s", line)
			}
		}

		if err := scanner.Err(); err != nil {
			log.Printf("[watchLogs] scanner error: %v", err)
		}

		_ = cmd.Wait()
		log.Printf("[watchLogs] journalctl exited — retrying in 5s")
		time.Sleep(5 * time.Second)
	}
}

// pollServiceStatus periodically checks whether the systemd service is active
// and updates the serviceUp gauge accordingly.
func pollServiceStatus(service string) {
	for {
		cmd := exec.Command("systemctl", "is-active", "--quiet", service)
		err := cmd.Run()
		if err == nil {
			serviceUp.Set(1)
		} else {
			serviceUp.Set(0)
		}
		time.Sleep(30 * time.Second)
	}
}

func main() {
	flag.Parse()

	go watchLogs(*serviceName)
	go pollServiceStatus(*serviceName)

	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	log.Printf("aqa-publisher-exporter listening on %s", *listenAddr)
	if err := http.ListenAndServe(*listenAddr, nil); err != nil {
		log.Fatalf("HTTP server error: %v", err)
	}
}
