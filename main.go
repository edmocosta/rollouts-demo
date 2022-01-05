package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	newrelic "github.com/newrelic/go-agent/v3/newrelic"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	// defaultTerminationDelay delays termination of the program in a graceful shutdown situation.
	// We do this to prevent the pod from exiting immediately upon a pod termination event
	// (e.g. during a rolling update). This gives some time for ingress controllers to react to
	// the Pod IP being removed from the Service's Endpoint list, which prevents traffic from being
	// directed to terminated pods, which otherwise would cause timeout errors and/or request delays.
	// See: See: https://github.com/kubernetes/ingress-nginx/issues/3335#issuecomment-434970950
	defaultTerminationDelay = 10
)

var (
	color  = os.Getenv("COLOR")
	colors = []string{
		"red",
		"orange",
		"yellow",
		"green",
		"blue",
		"purple",
	}
	envErrorRate = os.Getenv("ERROR_RATE")
	envLatency   = os.Getenv("LATENCY")
)

func main() {
	var app *newrelic.Application
	var err error

	useEnvConfig := os.Getenv("NEW_RELIC_USE_ENV_CONFIG")
	if useEnvConfig == "true" {
		app, err = newrelic.NewApplication(newrelic.ConfigFromEnvironment())
	} else {
		app, err = newrelic.NewApplication(
			newrelic.ConfigDebugLogger(os.Stdout),
			newrelic.ConfigEnabled(true),
			newrelic.ConfigDistributedTracerEnabled(true),
			newrelic.ConfigLicense(os.Getenv("NEW_RELIC_LICENSE_KEY")),
			newrelic.ConfigAppName("connect-service-cell-app"),
			func(cfg *newrelic.Config) {
				cfg.ErrorCollector.Enabled = true
				cfg.ErrorCollector.RecordPanics = true
				cfg.ErrorCollector.CaptureEvents = true
				cfg.ErrorCollector.Attributes.Enabled = true
				cfg.TransactionTracer.Enabled = true
				cfg.TransactionTracer.Attributes.Enabled = true
				cfg.CustomInsightsEvents.Enabled = true
				cfg.Utilization.DetectKubernetes = true
				cfg.Transport = &http.Transport{
					TLSClientConfig: &tls.Config{
						InsecureSkipVerify: true,
					},
				}

				if envLabels := os.Getenv("NEW_RELIC_LABELS"); envLabels != "" {
					if labels := getLabels(envLabels); len(labels) > 0 {
						cfg.Labels = labels
					} else {
						cfg.Error = fmt.Errorf("invalid NEW_RELIC_LABELS value: %s", envLabels)
					}
				}
			})
	}

	if nil != err {
		fmt.Println(err)
		os.Exit(1)
	}

	var (
		listenAddr       string
		terminationDelay int
		numCPUBurn       string
	)
	flag.StringVar(&listenAddr, "listen-addr", ":8080", "server listen address")
	flag.IntVar(&terminationDelay, "termination-delay", defaultTerminationDelay, "termination delay in seconds")
	flag.StringVar(&numCPUBurn, "cpu-burn", "", "burn specified number of cpus (number or 'all')")
	flag.Parse()

	rand.Seed(time.Now().UnixNano())

	router := http.NewServeMux()
	router.Handle("/", http.StripPrefix("/", http.FileServer(http.Dir("./"))))
	router.HandleFunc(newrelic.WrapHandleFunc(app, "/color", getColor))

	server := &http.Server{
		Addr:    listenAddr,
		Handler: router,
	}

	done := make(chan bool)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-quit
		server.SetKeepAlivesEnabled(false)
		log.Printf("Signal %v caught. Shutting down in %vs", sig, terminationDelay)
		delay := time.NewTicker(time.Duration(terminationDelay) * time.Second)
		defer delay.Stop()
		select {
		case <-quit:
			log.Println("Second signal caught. Shutting down NOW")
		case <-delay.C:
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			log.Fatalf("Could not gracefully shutdown the server: %v\n", err)
		}
		close(done)
	}()

	cpuBurn(done, numCPUBurn)
	log.Printf("Started server on %s", listenAddr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Could not listen on %s: %v\n", listenAddr, err)
	}

	<-done
	log.Println("Server stopped")
}

type colorParameters struct {
	Color            string `json:"color"`
	DelayProbability *int   `json:"delayPercent,omitempty"`
	DelayLength      int    `json:"delayLength,omitempty"`

	Return500Probability *int `json:"return500,omitempty"`
}

func getLabels(env string) map[string]string {
	out := make(map[string]string)
	env = strings.Trim(env, ";\t\n\v\f\r ")
	for _, entry := range strings.Split(env, ";") {
		if entry == "" {
			return nil
		}
		split := strings.Split(entry, ":")
		if len(split) != 2 {
			return nil
		}
		left := strings.TrimSpace(split[0])
		right := strings.TrimSpace(split[1])
		if left == "" || right == "" {
			return nil
		}
		if utf8.RuneCountInString(left) > 255 {
			runes := []rune(left)
			left = string(runes[:255])
		}
		if utf8.RuneCountInString(right) > 255 {
			runes := []rune(right)
			right = string(runes[:255])
		}
		out[left] = right
		if len(out) >= 64 {
			return out
		}
	}
	return out
}

func getColor(w http.ResponseWriter, r *http.Request) {
	requestBody, err := ioutil.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(500)
		log.Println(err.Error())
		fmt.Fprintf(w, err.Error())
		return
	}

	var request []colorParameters
	if len(requestBody) > 0 && string(requestBody) != `"[]"` {
		err = json.Unmarshal(requestBody, &request)
		if err != nil {
			w.WriteHeader(500)
			log.Printf("%s: %v", string(requestBody), err.Error())
			fmt.Fprintf(w, err.Error())
			return
		}
	}

	colorToReturn := randomColor()
	if color != "" {
		colorToReturn = color
	}

	var colorParams colorParameters
	for i := range request {
		cp := request[i]
		if cp.Color == colorToReturn {
			colorParams = cp
		}
	}

	if envLatency != "" {
		latency, err := strconv.Atoi(envLatency)
		if err != nil {
			w.WriteHeader(500)
			log.Printf("%s: %v", string(requestBody), err.Error())
			fmt.Fprintf(w, err.Error())
			return
		}
		log.Printf("Delaying %s %ds", colorToReturn, latency)
		time.Sleep(time.Duration(latency) * time.Second)
	} else if colorParams.DelayProbability != nil && *colorParams.DelayProbability > 0 && *colorParams.DelayProbability >= rand.Intn(100) {
		log.Printf("Delaying %s %ds", colorToReturn, colorParams.DelayLength)
		time.Sleep(time.Duration(colorParams.DelayLength) * time.Second)
	}

	returnSuccess := true
	if envErrorRate != "" {
		errorRate, err := strconv.Atoi(envErrorRate)
		if err != nil {
			w.WriteHeader(500)
			log.Printf("%s: %v", string(requestBody), err.Error())
			fmt.Fprintf(w, err.Error())
			return
		}
		returnSuccess = rand.Intn(100) >= errorRate
	} else if colorParams.Return500Probability != nil && *colorParams.Return500Probability > 0 && *colorParams.Return500Probability >= rand.Intn(100) {
		returnSuccess = false
	}
	printColor(colorToReturn, w, returnSuccess)
}

func printColor(colorToPrint string, w http.ResponseWriter, healthy bool) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if healthy {
		w.WriteHeader(http.StatusOK)
	} else {
		log.Println("Returning 500")
		w.WriteHeader(500)
	}
	switch colorToPrint {
	case "":
		randomColor := randomColor()
		if healthy {
			log.Printf("Successful %s\n", randomColor)
		} else {
			log.Printf("500 - %s\n", randomColor)
		}
		fmt.Fprintf(w, "\"%s\"", randomColor)
	default:
		if healthy {
			log.Printf("Successful %s\n", colorToPrint)
		} else {
			log.Printf("500 - %s\n", colorToPrint)
		}
		fmt.Fprintf(w, "\"%s\"", colorToPrint)
	}
}

func randomColor() string {
	return colors[rand.Int()%len(colors)]
}

func cpuBurn(done <-chan bool, numCPUBurn string) {
	if numCPUBurn == "" {
		return
	}
	var numCPU int
	if numCPUBurn == "all" {
		numCPU = runtime.NumCPU()
	} else {
		num, err := strconv.Atoi(numCPUBurn)
		if err != nil {
			log.Fatal(err)
		}
		numCPU = num
	}
	log.Printf("Burning %d CPUs", numCPU)
	noop := func() {}
	for i := 0; i < numCPU; i++ {
		go func(cpu int) {
			log.Printf("Burning CPU #%d", cpu)
			for {
				select {
				case <-done:
					log.Printf("Stopped CPU burn #%d", cpu)
					return
				default:
					noop()
				}
			}
		}(i)
	}
}
