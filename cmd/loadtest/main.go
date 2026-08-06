// Command loadtest is the wirefan load generator used by scripts/bench.sh.
//
// Design notes that matter for honest numbers:
//
//   - KEY POOL. The server rate-limits publishes PER API KEY (token bucket,
//     see internal/ratelimit). Sharing one key across N connections measures
//     the token bucket, not the fan-out engine. Pass a comma-separated pool
//     via --keys; connections are assigned round-robin (conn i uses
//     keys[i%len(keys)]). Size the pool so each key's aggregate publish rate
//     stays under the server's per-key limit.
//   - VISIBLE FAILURES. Every dial attempt is counted. attempted, connected,
//     and dial-failed totals are printed, as are server-sent error frames
//     bucketed by code (RATE_LIMITED, RATE_LIMITED_CONN, ...). A run where
//     half the connections silently died is distinguishable from a clean one.
//   - EXIT CODE. Exits nonzero when nothing connected, when any dials failed,
//     when any established connection (publisher or subscriber) died before
//     the duration elapsed, when the run produced zero throughput, or when
//     the server rejected messages. Harness scripts treat nonzero as "do not
//     publish".
//
// Publisher selection is deterministic (Bresenham over the within-key index)
// so every key group carries the same publisher fraction; random selection
// could overload one key's bucket by chance.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

var (
	addr       = flag.String("addr", "ws://localhost:8080", "server address")
	apiKeys    = flag.String("keys", "", "comma-separated API key id pool (required; round-robined across connections)")
	apiKeyOne  = flag.String("key", "", "single API key id (legacy alias for --keys with one entry)")
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

// counters aggregates run-wide tallies. All fields are atomics because every
// connection goroutine updates them concurrently.
type counters struct {
	attempted  atomic.Int64 // dial attempts started
	connected  atomic.Int64 // dials that produced a usable WebSocket
	dialFailed atomic.Int64 // dial or handshake errors
	subFailed  atomic.Int64 // subscribe that did not get a "subscribed" ack
	diedEarly  atomic.Int64 // established conns whose socket failed before the duration elapsed
	sent       atomic.Int64 // publish frames written to the socket
	recv       atomic.Int64 // "event" frames received

	errMu     sync.Mutex
	errByCode map[string]int64 // server "error" frames, bucketed by code

	dialErrMu    sync.Mutex
	dialErrKinds map[string]int64 // first line of each distinct dial error
}

func (c *counters) countErr(code string) {
	c.errMu.Lock()
	c.errByCode[code]++
	c.errMu.Unlock()
}

func (c *counters) countDialErr(err error) {
	msg := err.Error()
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	c.dialErrMu.Lock()
	c.dialErrKinds[msg]++
	c.dialErrMu.Unlock()
}

func main() {
	flag.Parse()
	pool := parseKeyPool()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	runCtx, runCancel := context.WithTimeout(ctx, *duration+*rampUp+15*time.Second)
	defer runCancel()

	ctrs := &counters{
		errByCode:    make(map[string]int64),
		dialErrKinds: make(map[string]int64),
	}

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
	fmt.Fprintf(os.Stderr, "dialing %d conns over %s rampup, %d channels, %.1f msg/s/conn (%.0f%% publishers), %d-key pool, duration %s\n",
		*conns, *rampUp, *channels, *rate, *publishers*100, len(pool), *duration)

	perConnDelay := *rampUp / time.Duration(*conns+1)
	if perConnDelay < time.Microsecond {
		perConnDelay = time.Microsecond
	}

	for i := 0; i < *conns; i++ {
		time.Sleep(perConnDelay)
		wg.Add(1)
		key := pool[i%len(pool)]
		// Deterministic publisher selection, stratified per key group: the
		// within-key index advances Bresenham-style so every key carries the
		// same publisher fraction (random selection could overload one key's
		// token bucket by chance).
		// When the pool is large enough that a key group holds fewer than two
		// conns, the within-key index is always 0 and this would select zero
		// publishers. Stratify across the global index instead; a group that
		// small cannot overload its own token bucket anyway.
		seq := i / len(pool)
		if *conns/len(pool) < 2 {
			seq = i
		}
		isPublisher := int(float64(seq+1)**publishers) > int(float64(seq)**publishers)
		chName := fmt.Sprintf("loadtest-%d", i%*channels)
		connURL := fmt.Sprintf("%s/v1/connect?key=%s", *addr, key)
		go runConn(runCtx, &wg, connURL, chName, isPublisher, *rate, *duration, ctrs, samples)
	}

	wg.Wait()
	close(samples)
	lats := <-samplesDone

	report(ctrs, lats)
}

func parseKeyPool() []string {
	raw := *apiKeys
	if raw == "" {
		raw = *apiKeyOne
	}
	var pool []string
	for _, k := range strings.Split(raw, ",") {
		k = strings.TrimSpace(k)
		if k != "" {
			pool = append(pool, k)
		}
	}
	if len(pool) == 0 {
		log.Fatal("--keys is required (comma-separated api key ids from POST /v1/keys)")
	}
	return pool
}

func report(ctrs *counters, lats []int64) {
	attempted := ctrs.attempted.Load()
	connected := ctrs.connected.Load()
	dialFailed := ctrs.dialFailed.Load()
	subFailed := ctrs.subFailed.Load()
	diedEarly := ctrs.diedEarly.Load()
	sentN := ctrs.sent.Load()
	recvN := ctrs.recv.Load()

	var errTotal int64
	var errLines []string
	ctrs.errMu.Lock()
	codes := make([]string, 0, len(ctrs.errByCode))
	for code := range ctrs.errByCode {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	for _, code := range codes {
		errTotal += ctrs.errByCode[code]
		errLines = append(errLines, fmt.Sprintf("server errors [%s]: %d", code, ctrs.errByCode[code]))
	}
	ctrs.errMu.Unlock()

	fmt.Println()
	fmt.Println("=== loadtest results ===")
	fmt.Printf("conns attempted:  %d\n", attempted)
	fmt.Printf("conns connected:  %d\n", connected)
	fmt.Printf("conns dial-fail:  %d\n", dialFailed)
	fmt.Printf("conns sub-fail:   %d\n", subFailed)
	fmt.Printf("conns died early: %d\n", diedEarly)
	ctrs.dialErrMu.Lock()
	for msg, n := range ctrs.dialErrKinds {
		fmt.Printf("  dial error x%d: %s\n", n, msg)
	}
	ctrs.dialErrMu.Unlock()
	fmt.Printf("channels:         %d\n", *channels)
	fmt.Printf("rate:             %.1f msg/s/conn (publishers: %.0f%%)\n", *rate, *publishers*100)
	fmt.Printf("duration:         %s\n", *duration)
	fmt.Printf("sent:             %d\n", sentN)
	fmt.Printf("recv:             %d\n", recvN)
	for _, l := range errLines {
		fmt.Println(l)
	}
	if sentN > 0 {
		// recv >> sent expected (each publish fans out to multiple subs)
		fmt.Printf("recv/sent:        %.2f\n", float64(recvN)/float64(sentN))
	}
	if *duration > 0 {
		fmt.Printf("delivered msg/s:  %.0f\n", float64(recvN)/(*duration).Seconds())
	}

	// Client-observed latency is quantized at the host wall-clock tick
	// (~0.5ms on Windows). Measure and print the resolution so percentiles
	// at or below it are read as a floor, not a value.
	resNs := wallClockResolution()
	fmt.Printf("host clock res:   %.1fus (client latencies at/below this are floor-limited)\n", float64(resNs)/1e3)

	var p50us, p99us, p999us, maxus float64
	if len(lats) > 0 {
		sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
		p := func(q float64) float64 {
			idx := int(float64(len(lats))*q + 0.5)
			if idx >= len(lats) {
				idx = len(lats) - 1
			}
			if idx < 0 {
				idx = 0
			}
			return float64(lats[idx]) / 1e3 // ns -> us
		}
		p50us, p99us, p999us = p(0.50), p(0.99), p(0.999)
		maxus = float64(lats[len(lats)-1]) / 1e3
		fmt.Printf("samples:          %d\n", len(lats))
		fmt.Printf("latency p50:      %.1fus\n", p50us)
		fmt.Printf("latency p99:      %.1fus\n", p99us)
		fmt.Printf("latency p999:     %.1fus\n", p999us)
		fmt.Printf("latency max:      %.1fus\n", maxus)
	}

	// Single machine-parseable line for harness scripts.
	fmt.Printf("SUMMARY attempted=%d connected=%d dial_failed=%d sub_failed=%d died_early=%d sent=%d recv=%d server_errors=%d delivered_per_s=%.0f p50_us=%.1f p99_us=%.1f p999_us=%.1f max_us=%.1f\n",
		attempted, connected, dialFailed, subFailed, diedEarly, sentN, recvN, errTotal,
		float64(recvN)/(*duration).Seconds(), p50us, p99us, p999us, maxus)

	switch {
	case connected == 0:
		fmt.Fprintln(os.Stderr, "FAIL: zero connections established")
		os.Exit(2)
	case dialFailed > 0 || subFailed > 0:
		fmt.Fprintln(os.Stderr, "FAIL: some connections failed to establish; not a clean run")
		os.Exit(3)
	case diedEarly > 0:
		fmt.Fprintln(os.Stderr, "FAIL: established connections died before the duration elapsed; not a clean run")
		os.Exit(7)
	case *publishers > 0 && sentN == 0:
		fmt.Fprintln(os.Stderr, "FAIL: zero messages sent; not a clean run")
		os.Exit(4)
	case errTotal > 0:
		fmt.Fprintln(os.Stderr, "FAIL: server rejected messages (see error counts); not a clean run")
		os.Exit(5)
	case recvN == 0:
		fmt.Fprintln(os.Stderr, "FAIL: zero messages received; not a clean run")
		os.Exit(6)
	}
}

// wallClockResolution returns the smallest observed nonzero delta between
// consecutive time.Now().UnixNano() reads: the effective quantum of every
// client-side latency sample. On Windows this is the interrupt-time tick
// (~0.5ms); on Linux it is nanoseconds.
func wallClockResolution() int64 {
	minD := int64(1) << 62
	deadline := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(deadline) {
		a := time.Now().UnixNano()
		b := time.Now().UnixNano()
		if d := b - a; d > 0 && d < minD {
			minD = d
		}
	}
	if minD == int64(1)<<62 {
		return 0
	}
	return minD
}

func runConn(ctx context.Context, wg *sync.WaitGroup, url, channel string, isPublisher bool, rate float64, duration time.Duration, ctrs *counters, samples chan<- sample) {
	defer wg.Done()
	ctrs.attempted.Add(1)
	dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)
	c, _, err := websocket.Dial(dialCtx, url, nil)
	dialCancel()
	if err != nil {
		ctrs.dialFailed.Add(1)
		ctrs.countDialErr(err)
		return
	}
	defer func() { _ = c.Close(websocket.StatusNormalClosure, "") }()

	// Drop the `connected` hello
	{
		readCtx, readCancel := context.WithTimeout(ctx, 10*time.Second)
		_, _, err := c.Read(readCtx)
		readCancel()
		if err != nil {
			ctrs.subFailed.Add(1)
			return
		}
	}

	// Subscribe and require the ack. An "error" frame here (rate limited,
	// channel cap) means this conn is not participating; count it.
	sub := map[string]string{"type": "subscribe", "channel": channel}
	b, _ := json.Marshal(sub)
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		ctrs.subFailed.Add(1)
		return
	}
	{
		// The ack is not guaranteed to be the first frame back. hub.Subscribe
		// registers this conn before the ack is enqueued, and only
		// per-subscriber FIFO is promised, so a concurrent publisher's event
		// frame on an already-busy channel can legitimately land first. Read
		// until the ack or an error frame rather than judging one frame.
		readCtx, readCancel := context.WithTimeout(ctx, 10*time.Second)
		var ack struct {
			Type string `json:"type"`
			Code string `json:"code"`
		}
		for {
			_, raw, err := c.Read(readCtx)
			if err != nil {
				readCancel()
				ctrs.subFailed.Add(1)
				return
			}
			ack.Type, ack.Code = "", ""
			if json.Unmarshal(raw, &ack) != nil {
				continue
			}
			if ack.Type == "subscribed" {
				break
			}
			if ack.Type == "error" {
				readCancel()
				ctrs.subFailed.Add(1)
				if ack.Code != "" {
					ctrs.countErr(ack.Code)
				}
				return
			}
			// Anything else is in-band data that beat the ack; keep reading.
		}
		readCancel()
	}
	ctrs.connected.Add(1)

	// Reader goroutine measures latency and tallies server error frames.
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
				Code    string          `json:"code"`
				Channel string          `json:"channel"`
				Data    json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(raw, &msg); err != nil {
				continue
			}
			switch msg.Type {
			case "error":
				ctrs.countErr(msg.Code)
				continue
			case "event":
			default:
				continue
			}
			ctrs.recv.Add(1)
			// Extract the publish timestamp embedded in data
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
					// A write error before the duration elapsed means this
					// established conn died mid-run (server crash, container
					// teardown, network reset). Silent truncation here would
					// deflate throughput invisibly, so it is counted and
					// fails the run.
					if pubCtx.Err() == nil && ctx.Err() == nil {
						ctrs.diedEarly.Add(1)
					}
					goto done
				}
				ctrs.sent.Add(1)
			}
		}
	} else {
		// Subscriber-only, wait out the duration. The reader goroutine
		// closes readDone when c.Read fails; before the duration elapses
		// that means the socket died mid-run (server crash, container
		// teardown, network reset). Subscribers produce the entire recv
		// count, so a silent early death here would deflate delivered
		// throughput invisibly, exactly like a publisher write error.
		select {
		case <-ctx.Done():
		case <-readDone:
			if ctx.Err() == nil {
				ctrs.diedEarly.Add(1)
			}
		case <-time.After(duration):
		}
	}
done:
	_ = c.Close(websocket.StatusNormalClosure, "")
	<-readDone
}
