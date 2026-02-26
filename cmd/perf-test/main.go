package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

type WebSocketMessage struct {
	Event string `json:"event"`
}

var (
	targetURL   string
	conns       int
	rate        int
	duration    time.Duration
	mode        string
	success     atomic.Uint64
	failures    atomic.Uint64
	activeConns atomic.Int64
)

func main() {
	flag.StringVar(&targetURL, "url", "ws://localhost:8080/mock/websocket", "Websocket URL to test")
	flag.IntVar(&conns, "c", 100, "Number of concurrent connections (for active mode)")
	flag.IntVar(&rate, "r", 10, "New connections per second (for rate mode)")
	flag.DurationVar(&duration, "d", 10*time.Second, "Duration of the test")
	flag.StringVar(&mode, "mode", "active", "Test mode: 'active' or 'rate'")
	flag.Parse()

	log.Printf("Starting test: mode=%s, url=%s, duration=%s", mode, targetURL, duration)

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	go func() {
		<-interrupt
		log.Println("Interrupted, shutting down...")
		os.Exit(0)
	}()

	switch mode {
	case "active":
		runActiveTest()
	case "rate":
		runRateTest()
	default:
		log.Fatalf("Unknown mode: %s. Use 'active' or 'rate'", mode)
	}

	log.Printf("Test complete. Success: %d, Failures: %d", success.Load(), failures.Load())
}

func runActiveTest() {
	u, err := url.Parse(targetURL)
	if err != nil {
		log.Fatal("Invalid URL:", err)
	}

	var wg sync.WaitGroup
	start := time.Now()

	var latencies []time.Duration
	var latenciesLock sync.Mutex

	log.Printf("Ramping up to %d connections...", conns)

	// Ramp up
	for i := 0; i < conns; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			connStart := time.Now()
			c, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
			if err != nil {
				failures.Add(1)
				log.Printf("Connect error (client %d): %v", id, err)
				return
			}
			defer c.Close()

			// Wait for first message to measure latency
			_, message, err := c.ReadMessage()
			if err != nil {
				failures.Add(1)
				log.Printf("Read error (client %d): %v", id, err)
				return
			}

			var msg WebSocketMessage
			if err := json.Unmarshal(message, &msg); err != nil {
				failures.Add(1)
				log.Printf("JSON parse error (client %d): %v", id, err)
				return
			}
			if msg.Event != "sync" {
				failures.Add(1)
				log.Printf("Unexpected event (client %d): got %s, want sync", id, msg.Event)
				return
			}

			latency := time.Since(connStart)
			latenciesLock.Lock()
			latencies = append(latencies, latency)
			latenciesLock.Unlock()

			success.Add(1)
			activeConns.Add(1)
			defer activeConns.Add(-1)

			// Hold connection
			done := make(chan struct{})

			// Read loop to keep connection alive and handle server close
			go func() {
				defer close(done)
				for {
					_, _, err := c.ReadMessage()
					if err != nil {
						return
					}
				}
			}()

			// Wait for duration or error
			select {
			case <-time.After(duration):
			case <-done:
			}
		}(i)

		// Small delay to prevent local port exhaustion/syn flood issues during ramp up if count is huge
		time.Sleep(10 * time.Millisecond)
	}

	log.Printf("Ramp up complete. Holding active connections...")

	// Wait for remaining duration if ramp up was fast
	remaining := time.Until(start.Add(duration))
	if remaining > 0 {
		time.Sleep(remaining)
	}

	// Wait for all goroutines to finish
	wg.Wait()

	// Calculate and print stats
	if len(latencies) > 0 {
		var total time.Duration
		var min = latencies[0]
		var max = latencies[0]

		for _, l := range latencies {
			total += l
			if l < min {
				min = l
			}
			if l > max {
				max = l
			}
		}
		avg := total / time.Duration(len(latencies))

		log.Printf("Latency Stats (Connect -> First Message):")
		log.Printf("  Min: %v", min)
		log.Printf("  Max: %v", max)
		log.Printf("  Avg: %v", avg)
	}
}

func runRateTest() {
	u, err := url.Parse(targetURL)
	if err != nil {
		log.Fatal("Invalid URL:", err)
	}

	ticker := time.NewTicker(time.Second / time.Duration(rate))
	if rate == 0 {
		ticker = time.NewTicker(time.Millisecond)
	}
	defer ticker.Stop()

	stop := time.After(duration)
	var wg sync.WaitGroup

	var latencies []time.Duration
	var latenciesLock sync.Mutex

	log.Printf("Starting rate test: %d conns/sec", rate)

loop:
	for {
		select {
		case <-stop:
			break loop
		case <-ticker.C:
			wg.Go(func() {
				start := time.Now()
				c, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
				if err != nil {
					failures.Add(1)
					log.Printf("Connect error: %v", err)
					return
				}
				defer c.Close()

				// Wait for first message
				_, message, err := c.ReadMessage()
				if err != nil {
					failures.Add(1)
					log.Printf("Read error: %v", err)
					return
				}

				var msg WebSocketMessage
				if err := json.Unmarshal(message, &msg); err != nil {
					failures.Add(1)
					log.Printf("JSON parse error: %v", err)
					return
				}
				if msg.Event != "sync" {
					failures.Add(1)
					log.Printf("Unexpected event: got %s, want sync", msg.Event)
					return
				}

				latency := time.Since(start)
				latenciesLock.Lock()
				latencies = append(latencies, latency)
				latenciesLock.Unlock()

				success.Add(1)
			})
		}
	}
	wg.Wait()

	// Calculate and print stats
	if len(latencies) > 0 {
		var total time.Duration
		var min = latencies[0]
		var max = latencies[0]

		for _, l := range latencies {
			total += l
			if l < min {
				min = l
			}
			if l > max {
				max = l
			}
		}
		avg := total / time.Duration(len(latencies))

		log.Printf("Latency Stats (Connect -> First Message):")
		log.Printf("  Min: %v", min)
		log.Printf("  Max: %v", max)
		log.Printf("  Avg: %v", avg)
	}
}
