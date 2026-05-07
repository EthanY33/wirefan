package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

var (
	addr       = flag.String("addr", "ws://localhost:8080", "server address")
	apiKey     = flag.String("key", "", "API key id (required)")
	conns      = flag.Int("conns", 1000, "concurrent connections")
	channels   = flag.Int("channels", 100, "distinct channels (clients distributed across them)")
	rate       = flag.Float64("rate", 10, "msg/s per publishing connection")
	duration   = flag.Duration("dur", 30*time.Second, "test duration")
	rampUp     = flag.Duration("rampup", 5*time.Second, "ramp up dial spread")
	publishers = flag.Float64("publishers", 0.5, "fraction of conns that publish (0..1)")
)

type sample struct {
	latencyNs int64
}

func main() {
	flag.Parse()
	if *apiKey == "" {
		log.Fatal("--key is required (api key id from /v1/keys)")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	runCtx, runCancel := context.WithTimeout(ctx, *duration+*rampUp+5*time.Second)
	defer runCancel()

	var sent, recv atomic.Int64

	samples := make(chan sample, *conns*16)
	samplesDone := make(chan []int64, 1)
	go func() {
		var lats []int64
		for s := range samples {
			lats = append(lats, s.latencyNs)
		}
		samplesDone <- lats
	}()

	var wg sync.WaitGroup
	fmt.Fprintf(os.Stderr, "dialing %d conns over %s rampup, %d channels, %.1f msg/s/conn (%.0f%% publishers), duration %s\n",
		*conns, *rampUp, *channels, *rate, *publishers*100, *duration)

	perConnDelay := *rampUp / time.Duration(*conns+1)
	if perConnDelay < time.Microsecond {
		perConnDelay = time.Microsecond
	}
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	for i := 0; i < *conns; i++ {
		time.Sleep(perConnDelay)
		wg.Add(1)
		isPublisher := rng.Float64() < *publishers
		chName := fmt.Sprintf("loadtest-%d", i%*channels)
		connURL := fmt.Sprintf("%s/v1/connect?key=%s", *addr, *apiKey)
		go runConn(runCtx, &wg, connURL, chName, isPublisher, *rate, *duration, &sent, &recv, samples)
	}

	wg.Wait()
	close(samples)
	lats := <-samplesDone

	sentN, recvN := sent.Load(), recv.Load()
	fmt.Println()
	fmt.Println("=== loadtest results ===")
	fmt.Printf("conns:        %d\n", *conns)
	fmt.Printf("channels:     %d\n", *channels)
	fmt.Printf("rate:         %.1f msg/s/conn (publishers: %.0f%%)\n", *rate, *publishers*100)
	fmt.Printf("duration:     %s\n", *duration)
	fmt.Printf("sent:         %d\n", sentN)
	fmt.Printf("recv:         %d\n", recvN)
	if sentN > 0 {
		// recv >> sent expected (each publish fans out to multiple subs); show ratio
		fmt.Printf("recv/sent:    %.2f\n", float64(recvN)/float64(sentN))
	}
	if len(lats) > 0 {
		sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
		p := func(q float64) time.Duration {
			idx := int(float64(len(lats))*q + 0.5)
			if idx >= len(lats) {
				idx = len(lats) - 1
			}
			if idx < 0 {
				idx = 0
			}
			return time.Duration(lats[idx])
		}
		fmt.Printf("samples:      %d\n", len(lats))
		fmt.Printf("latency p50:  %s\n", p(0.50))
		fmt.Printf("latency p99:  %s\n", p(0.99))
		fmt.Printf("latency p999: %s\n", p(0.999))
		fmt.Printf("latency max:  %s\n", time.Duration(lats[len(lats)-1]))
	}
}

func runConn(ctx context.Context, wg *sync.WaitGroup, url, channel string, isPublisher bool, rate float64, duration time.Duration, sent, recv *atomic.Int64, samples chan<- sample) {
	defer wg.Done()
	dialCtx, dialCancel := context.WithTimeout(ctx, 5*time.Second)
	c, _, err := websocket.Dial(dialCtx, url, nil)
	dialCancel()
	if err != nil {
		return
	}
	defer func() { _ = c.Close(websocket.StatusNormalClosure, "") }()

	// Drop the `connected` hello
	{
		readCtx, readCancel := context.WithTimeout(ctx, 5*time.Second)
		_, _, _ = c.Read(readCtx)
		readCancel()
	}

	// Subscribe
	sub := map[string]string{"type": "subscribe", "channel": channel}
	b, _ := json.Marshal(sub)
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		return
	}
	// Drop the `subscribed` ack
	{
		readCtx, readCancel := context.WithTimeout(ctx, 5*time.Second)
		_, _, _ = c.Read(readCtx)
		readCancel()
	}

	// Reader goroutine — measures latency
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			_, raw, err := c.Read(ctx)
			if err != nil {
				return
			}
			var msg struct {
				Type    string          `json:"type"`
				Channel string          `json:"channel"`
				Data    json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(raw, &msg); err != nil {
				continue
			}
			if msg.Type != "event" {
				continue
			}
			recv.Add(1)
			// Try to extract the publish timestamp embedded in data
			var d struct {
				T int64 `json:"t"`
			}
			if json.Unmarshal(msg.Data, &d) == nil && d.T > 0 {
				select {
				case samples <- sample{latencyNs: time.Now().UnixNano() - d.T}:
				default:
				}
			}
		}
	}()

	if isPublisher {
		// Publish at the configured rate
		interval := time.Duration(float64(time.Second) / rate)
		pubCtx, pubCancel := context.WithTimeout(ctx, duration)
		defer pubCancel()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-pubCtx.Done():
				goto done
			case <-ticker.C:
				payload, _ := json.Marshal(map[string]any{
					"type":    "publish",
					"channel": channel,
					"data":    map[string]int64{"t": time.Now().UnixNano()},
				})
				if err := c.Write(ctx, websocket.MessageText, payload); err != nil {
					goto done
				}
				sent.Add(1)
			}
		}
	} else {
		// Subscriber-only — wait for duration
		select {
		case <-ctx.Done():
		case <-time.After(duration):
		}
	}
done:
	_ = c.Close(websocket.StatusNormalClosure, "")
	<-readDone
}
