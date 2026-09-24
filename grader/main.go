// Firefly Concert grader. Go 1.22+, standard library only.
//
// Seven correctness groups (10 points each), then a load phase that offers a fixed request
// rate open-loop (10,000 requests/second by default) across many keep-alive connections.
// The load phase repeats (30 runs by default); speed points (up to 30) are the average of
// each run's score against the offered rate and a p99 latency target, and are awarded only
// if every correctness group passes, including under load in every run.
//
//	firefly-grader --url http://127.0.0.1:8765 --correctness-only
//	firefly-grader --url http://127.0.0.1:8765 --runs 1          # quick feedback
//	firefly-grader --url http://127.0.0.1:8765 --output report.json
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"os"
	"reflect"
	"runtime"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

const cells = 64 * 64

// ---------------------------------------------------------------------------------------
// HTTP client: one connection per client, at most one request outstanding.

type client struct {
	base string
	http *http.Client
}

func newClient(base string) *client {
	transport := &http.Transport{
		MaxIdleConns:        1,
		MaxIdleConnsPerHost: 1,
		MaxConnsPerHost:     1,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	}
	return &client{base: strings.TrimRight(base, "/"), http: &http.Client{Transport: transport, Timeout: 30 * time.Second}}
}

func (c *client) close() { c.http.CloseIdleConnections() }

func (c *client) do(method, path string, body []byte) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	if ct := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]); ct != "application/json" {
		return resp.StatusCode, data, errors.New("Missing JSON Content-Type")
	}
	if !json.Valid(data) {
		return resp.StatusCode, data, fmt.Errorf("%s %s: response is not valid JSON", method, path)
	}
	return resp.StatusCode, data, nil
}

// decodeAny parses JSON keeping numbers as their exact tokens, so 0 and 0.0 differ.
func decodeAny(data []byte) any {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return nil
	}
	return v
}

func short(data []byte) string {
	if len(data) > 200 {
		return string(data[:200]) + "…"
	}
	return string(data)
}

// expect sends a request and requires an exact status and JSON body.
func (c *client) expect(method, path string, body []byte, status int, want string) error {
	got, data, err := c.do(method, path, body)
	if err != nil {
		return err
	}
	if got != status || !reflect.DeepEqual(decodeAny(data), decodeAny([]byte(want))) {
		return fmt.Errorf("%s %s: expected (%d, %s), got (%d, %s)", method, path, status, want, got, short(data))
	}
	return nil
}

func (c *client) create(wall string) error {
	body, _ := json.Marshal(map[string]string{"wall_id": wall})
	want, _ := json.Marshal(map[string]any{"wall_id": wall, "version": 0})
	return c.expect("POST", "/walls", body, 201, string(want))
}

type snapshot struct {
	Version int64
	Pixels  []int64
}

func isNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

// parseSnapshot enforces the exact schema: two keys, integer version, 4,096 integer pixels.
// Go's decoder rejects 1.0, 1e0, true and "1" for int64 targets; null is checked by hand
// because decoding null into an int64 is silently a no-op.
func parseSnapshot(status int, data []byte) (snapshot, error) {
	var s snapshot
	var fields map[string]json.RawMessage
	if status != 200 || json.Unmarshal(data, &fields) != nil || len(fields) != 2 ||
		fields["version"] == nil || fields["pixels"] == nil {
		return s, fmt.Errorf("Invalid snapshot status/schema: %d %s", status, short(data))
	}
	if isNull(fields["version"]) || json.Unmarshal(fields["version"], &s.Version) != nil || s.Version < 0 {
		return s, errors.New("Invalid snapshot version")
	}
	if bytes.Contains(fields["pixels"], []byte("null")) || json.Unmarshal(fields["pixels"], &s.Pixels) != nil {
		return s, errors.New("Pixels must be nonnegative integers")
	}
	if len(s.Pixels) != cells {
		return s, fmt.Errorf("Snapshot needs exactly 4096 pixels, got %d", len(s.Pixels))
	}
	for _, p := range s.Pixels {
		if p < 0 {
			return s, errors.New("Pixels must be nonnegative integers")
		}
	}
	return s, nil
}

func (c *client) snapshot(wall string) (snapshot, error) {
	status, data, err := c.do("GET", "/walls/"+wall, nil)
	if err != nil {
		return snapshot{}, err
	}
	return parseSnapshot(status, data)
}

func parseBurstReply(status int, data []byte) (int64, error) {
	var fields map[string]json.RawMessage
	var version int64
	if status != 200 || json.Unmarshal(data, &fields) != nil || len(fields) != 1 || fields["version"] == nil {
		return 0, fmt.Errorf("Invalid burst status/schema: %d %s", status, short(data))
	}
	if isNull(fields["version"]) || json.Unmarshal(fields["version"], &version) != nil || version <= 0 {
		return 0, errors.New("Invalid burst version")
	}
	return version, nil
}

type burst struct{ X, Y, Width, Height, Energy int }

func (b burst) json(extra string) []byte {
	return []byte(fmt.Sprintf(`{"x":%d,"y":%d,"width":%d,"height":%d,"energy":%d%s}`,
		b.X, b.Y, b.Width, b.Height, b.Energy, extra))
}

func (c *client) burst(wall string, b burst, extra string) (int64, error) {
	status, data, err := c.do("POST", "/walls/"+wall+"/bursts", b.json(extra))
	if err != nil {
		return 0, err
	}
	return parseBurstReply(status, data)
}

// oracleAdd applies a burst with a deliberately independent per-cell predicate.
func oracleAdd(pixels []int64, b burst) {
	for i := range cells {
		y, x := i/64, i%64
		if b.X <= x && x < b.X+b.Width && b.Y <= y && y < b.Y+b.Height {
			pixels[i] += int64(b.Energy)
		}
	}
}

func uniform(n int64) []int64 {
	p := make([]int64, cells)
	for i := range p {
		p[i] = n
	}
	return p
}

func expectState(s snapshot, version int64, pixels []int64, message string) error {
	if s.Version != version || !slices.Equal(s.Pixels, pixels) {
		return fmt.Errorf("%s (got version %d, expected %d)", message, s.Version, version)
	}
	return nil
}

// parallel runs fn on n goroutines released together, returning the first error.
func parallel(n int, fn func(i int) error) error {
	var start, done sync.WaitGroup
	start.Add(1)
	errs := make([]error, n)
	for i := range n {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			errs[i] = fn(i)
		}()
	}
	start.Done()
	done.Wait()
	return errors.Join(errs...)
}

func randomID() string {
	return fmt.Sprintf("%012x", rand.Int63()&0xffffffffffff)
}

// ---------------------------------------------------------------------------------------
// Correctness groups.

type check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Error  string `json:"error,omitempty"`
}

func functional(base string) []check {
	prefix := randomID()
	c := newClient(base)
	defer c.close()
	fresh := func(suffix string) (string, error) {
		wall := prefix + suffix
		return wall, c.create(wall)
	}

	empty := func() error {
		wall, err := fresh("empty")
		if err != nil {
			return err
		}
		s, err := c.snapshot(wall)
		if err != nil {
			return err
		}
		return expectState(s, 0, uniform(0), "New wall is not empty")
	}

	arithmetic := func() error {
		wall, err := fresh("arithmetic")
		if err != nil {
			return err
		}
		rng := rand.New(rand.NewSource(42))
		examples := []burst{{2, 3, 2, 2, 5}, {3, 3, 1, 1, 2}, {0, 0, 64, 64, 100}, {63, 63, 1, 1, 7}}
		for range 20 {
			x, y := rng.Intn(64), rng.Intn(64)
			examples = append(examples, burst{x, y, 1 + rng.Intn(64-x), 1 + rng.Intn(64-y), 1 + rng.Intn(100)})
		}
		examples = append(examples, examples[len(examples)-1])
		expected := make([]int64, cells)
		for i, b := range examples {
			version, err := c.burst(wall, b, `,"ignored":true`)
			if err != nil {
				return err
			}
			if version != int64(i+1) {
				return fmt.Errorf("Wrong sequential update version: got %d, expected %d", version, i+1)
			}
			oracleAdd(expected, b)
			s, err := c.snapshot(wall)
			if err != nil {
				return err
			}
			if err := expectState(s, int64(i+1), expected, fmt.Sprintf("Wrong pixel values after burst %d", i+1)); err != nil {
				return err
			}
		}
		return nil
	}

	validation := func() error {
		wall, err := fresh("invalid")
		if err != nil {
			return err
		}
		fields := []string{"x", "y", "width", "height", "energy"}
		valid := map[string]string{"x": "0", "y": "0", "width": "1", "height": "1", "energy": "1"}
		object := func(values map[string]string, skip string) []byte {
			parts := []string{}
			for _, f := range fields {
				if f != skip {
					parts = append(parts, fmt.Sprintf("%q:%s", f, values[f]))
				}
			}
			return []byte("{" + strings.Join(parts, ",") + "}")
		}
		invalid := [][]byte{[]byte("null"), []byte("[]"), []byte("{}"), []byte("{")}
		for _, f := range fields {
			invalid = append(invalid, object(valid, f))
			for _, bad := range []string{"true", "null", `"1"`, "1.0", "1e0", "-1"} {
				values := map[string]string{}
				for k, v := range valid {
					values[k] = v
				}
				values[f] = bad
				invalid = append(invalid, object(values, ""))
			}
		}
		for _, b := range []burst{{0, 0, 0, 1, 1}, {0, 0, 1, 0, 1}, {0, 0, 1, 1, 0}, {0, 0, 1, 1, 101}, {63, 0, 2, 1, 1}, {0, 63, 1, 2, 1}} {
			invalid = append(invalid, b.json(""))
		}
		for _, body := range invalid {
			if err := c.expect("POST", "/walls/"+wall+"/bursts", body, 400, `{"error":"invalid_request"}`); err != nil {
				return fmt.Errorf("%w (body %s)", err, body)
			}
		}
		for _, id := range []string{`""`, `"` + strings.Repeat("x", 65) + `"`, `"bad/id"`, "true", "null", "3"} {
			if err := c.expect("POST", "/walls", []byte(`{"wall_id":`+id+`}`), 400, `{"error":"invalid_request"}`); err != nil {
				return err
			}
		}
		s, err := c.snapshot(wall)
		if err != nil {
			return err
		}
		return expectState(s, 0, uniform(0), "Invalid request changed state")
	}

	isolation := func() error {
		a, err := fresh("a")
		if err != nil {
			return err
		}
		b, err := fresh("b")
		if err != nil {
			return err
		}
		if _, err := c.burst(a, burst{0, 0, 1, 1, 1}, ""); err != nil {
			return err
		}
		if err := c.expect("POST", "/walls", []byte(`{"wall_id":"`+a+`"}`), 409, `{"error":"already_exists"}`); err != nil {
			return err
		}
		want := uniform(0)
		want[0] = 1
		s, err := c.snapshot(a)
		if err != nil {
			return err
		}
		if err := expectState(s, 1, want, "Create reset existing wall"); err != nil {
			return err
		}
		if s, err = c.snapshot(b); err != nil {
			return err
		}
		if err := expectState(s, 0, uniform(0), "Walls are not isolated"); err != nil {
			return err
		}
		if err := c.expect("GET", "/walls/"+prefix+"missing", nil, 404, `{"error":"not_found"}`); err != nil {
			return err
		}
		if err := c.expect("POST", "/walls/"+prefix+"missing/bursts", []byte("{"), 404, `{"error":"not_found"}`); err != nil {
			return err
		}
		return c.expect("GET", "/unknown", nil, 404, `{"error":"not_found"}`)
	}

	creationRace := func() error {
		wall := prefix + "race"
		created, conflicts := 0, 0
		var mu sync.Mutex
		err := parallel(8, func(int) error {
			rc := newClient(base)
			defer rc.close()
			status, data, err := rc.do("POST", "/walls", []byte(`{"wall_id":"`+wall+`"}`))
			if err != nil {
				return err
			}
			got := decodeAny(data)
			mu.Lock()
			defer mu.Unlock()
			if status == 201 && reflect.DeepEqual(got, decodeAny([]byte(`{"wall_id":"`+wall+`","version":0}`))) {
				created++
			} else if status == 409 && reflect.DeepEqual(got, decodeAny([]byte(`{"error":"already_exists"}`))) {
				conflicts++
			}
			return nil
		})
		if err != nil {
			return err
		}
		if created != 1 || conflicts != 7 {
			return fmt.Errorf("Concurrent creates must produce one 201 and seven 409s (got %d and %d)", created, conflicts)
		}
		return nil
	}

	atomicity := func() error {
		wall, err := fresh("atomic")
		if err != nil {
			return err
		}
		versions := make([][]int64, 8)
		err = parallel(8, func(i int) error {
			wc := newClient(base)
			defer wc.close()
			var last int64
			for range 40 {
				if i < 4 {
					v, err := wc.burst(wall, burst{0, 0, 64, 64, 1}, "")
					if err != nil {
						return err
					}
					if v <= last {
						return errors.New("Burst version moved backward")
					}
					versions[i] = append(versions[i], v)
					last = v
				}
				s, err := wc.snapshot(wall)
				if err != nil {
					return err
				}
				if s.Version < last {
					return errors.New("Read omitted a completed operation")
				}
				for _, p := range s.Pixels {
					if p != s.Version {
						return errors.New("Torn snapshot: every pixel must equal version for full-wall unit bursts")
					}
				}
				last = s.Version
			}
			return nil
		})
		if err != nil {
			return err
		}
		all := slices.Concat(versions...)
		slices.Sort(all)
		for i, v := range all {
			if v != int64(i+1) || len(all) != 160 {
				return errors.New("Missing/repeated concurrent update versions")
			}
		}
		s, err := c.snapshot(wall)
		if err != nil {
			return err
		}
		return expectState(s, 160, uniform(160), "Lost concurrent updates")
	}

	groups := []struct {
		name string
		fn   func() error
	}{
		{"empty wall", empty},
		{"exact arithmetic", arithmetic},
		{"validation", validation},
		{"isolation and errors", isolation},
		{"concurrent creation", creationRace},
		{"atomic snapshots", atomicity},
	}
	var checks []check
	for _, g := range groups {
		ch := check{Name: g.name, Passed: true}
		if err := g.fn(); err != nil {
			ch.Passed, ch.Error = false, truncate(err.Error(), 1000)
		}
		checks = append(checks, ch)
	}
	return checks
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ---------------------------------------------------------------------------------------
// Load phase.

type latency struct {
	P50 *float64 `json:"p50_ms"`
	P95 *float64 `json:"p95_ms"`
	P99 *float64 `json:"p99_ms"`
}

type workloadResult struct {
	Walls           int      `json:"walls"`
	OfferedRPS      float64  `json:"offered_rps"`
	Requests        int      `json:"requests"`
	Seconds         float64  `json:"seconds"`
	SuccessfulRPS   float64  `json:"successful_rps"`
	ReadLatency     latency  `json:"read_latency"`
	WriteLatency    latency  `json:"write_latency"`
	Errors          int      `json:"errors"`
	ErrorExamples   []string `json:"error_examples"`
	ExactFinalState bool     `json:"exact_final_state"`
}

type job struct {
	offset time.Duration
	wall   int
	burst  *burst
}

func percentiles(values []time.Duration) latency {
	if len(values) == 0 {
		return latency{}
	}
	slices.Sort(values)
	at := func(p float64) *float64 {
		ms := math.Round(float64(values[int(math.Ceil(float64(len(values))*p/100))-1])/1e6*1e4) / 1e4
		return &ms
	}
	return latency{at(50), at(95), at(99)}
}

type connectionResult struct {
	reads, writes []time.Duration
	errors        []string
	versions      [][]int64
	done          time.Time
}

// drive sends one connection's requests on schedule and validates every reply.
//
// Latency normally runs from the moment the request is sent. When the previous reply on
// this connection arrived after this request was due, the server is behind, and latency runs
// from the scheduled time instead, so queueing counts against the server. Timer slack in
// the grader itself is never charged to the server.
func drive(c *client, jobs []job, walls []string, start time.Time) connectionResult {
	r := connectionResult{versions: make([][]int64, len(walls))}
	last := make([]int64, len(walls))
	previous := start
	for _, j := range jobs {
		due := start.Add(j.offset)
		if wait := time.Until(due); wait > 0 {
			time.Sleep(wait)
		}
		from := time.Now()
		if previous.After(due) {
			from = due
		}
		var status int
		var data []byte
		var err error
		if j.burst != nil {
			status, data, err = c.do("POST", "/walls/"+walls[j.wall]+"/bursts", j.burst.json(""))
		} else {
			status, data, err = c.do("GET", "/walls/"+walls[j.wall], nil)
		}
		previous = time.Now()
		elapsed := previous.Sub(from)
		if err == nil {
			if j.burst != nil {
				var v int64
				if v, err = parseBurstReply(status, data); err == nil {
					if v <= last[j.wall] {
						err = errors.New("Update version moved backward")
					} else {
						r.versions[j.wall] = append(r.versions[j.wall], v)
						last[j.wall] = v
						r.writes = append(r.writes, elapsed)
					}
				}
			} else {
				var s snapshot
				if s, err = parseSnapshot(status, data); err == nil {
					var sum int64
					for _, p := range s.Pixels {
						sum += p
					}
					switch {
					case s.Version < last[j.wall]:
						err = errors.New("Snapshot moved backward")
					case sum != 16*s.Version:
						err = errors.New("Snapshot energy/version mismatch")
					default:
						last[j.wall] = s.Version
						r.reads = append(r.reads, elapsed)
					}
				}
			}
		}
		if err != nil {
			r.errors = append(r.errors, truncate(err.Error(), 300))
		}
	}
	r.done = time.Now()
	return r
}

func workload(base string, rate float64, duration time.Duration, connections, wallCount int) (workloadResult, error) {
	control := newClient(base)
	defer control.close()
	prefix := randomID()
	walls := make([]string, wallCount)
	for i := range walls {
		walls[i] = fmt.Sprintf("%s%d", prefix, i)
		if err := control.create(walls[i]); err != nil {
			return workloadResult{}, err
		}
	}
	count := max(connections, int(math.Round(rate*duration.Seconds())))
	rng := rand.New(rand.NewSource(1701))
	jobs := make([][]job, connections)
	expected := make([][]int64, wallCount)
	counts := make([]int64, wallCount)
	for i := range expected {
		expected[i] = make([]int64, cells)
	}
	for i := range count {
		j := job{offset: time.Duration(float64(i) / rate * float64(time.Second)), wall: rng.Intn(wallCount)}
		if i%4 != 0 {
			j.burst = &burst{rng.Intn(61), rng.Intn(61), 4, 4, 1}
			oracleAdd(expected[j.wall], *j.burst)
			counts[j.wall]++
		}
		jobs[i%connections] = append(jobs[i%connections], j)
	}

	clients := make([]*client, connections)
	var ready sync.WaitGroup
	for i := range clients {
		clients[i] = newClient(base)
		ready.Add(1)
		go func() {
			defer ready.Done()
			clients[i].snapshot(walls[0]) // open and warm the connection
		}()
	}
	ready.Wait()
	start := time.Now().Add(100 * time.Millisecond)
	results := make([]connectionResult, connections)
	var done sync.WaitGroup
	for i := range clients {
		done.Add(1)
		go func() {
			defer done.Done()
			results[i] = drive(clients[i], jobs[i], walls, start)
			clients[i].close()
		}()
	}
	done.Wait()

	var reads, writes []time.Duration
	var errs []string
	end := start
	for _, r := range results {
		reads = append(reads, r.reads...)
		writes = append(writes, r.writes...)
		errs = append(errs, r.errors...)
		if r.done.After(end) {
			end = r.done
		}
	}
	exact := true
	for w, wall := range walls {
		s, err := control.snapshot(wall)
		exact = exact && err == nil && s.Version == counts[w] && slices.Equal(s.Pixels, expected[w])
		var versions []int64
		for _, r := range results {
			versions = append(versions, r.versions[w]...)
		}
		sort.Slice(versions, func(a, b int) bool { return versions[a] < versions[b] })
		exact = exact && int64(len(versions)) == counts[w]
		for i, v := range versions {
			exact = exact && v == int64(i+1)
		}
	}
	elapsed := end.Sub(start).Seconds()
	examples := errs[:min(3, len(errs))]
	if examples == nil {
		examples = []string{}
	}
	return workloadResult{
		Walls:           wallCount,
		OfferedRPS:      rate,
		Requests:        count,
		Seconds:         elapsed,
		SuccessfulRPS:   float64(len(reads)+len(writes)) / elapsed,
		ReadLatency:     percentiles(reads),
		WriteLatency:    percentiles(writes),
		Errors:          len(errs),
		ErrorExamples:   examples,
		ExactFinalState: exact,
	}, nil
}

// speedPoints is 30 × the geometric mean of six ratios capped at 1: per workload, achieved
// over offered throughput, and target over measured p99 for reads and for writes. Meeting
// the rate and the latency target in both workloads earns all 30; beating them earns nothing
// extra. A server that falls behind loses on throughput and again on queueing latency.
func speedPoints(runs []workloadResult, targetMs float64) float64 {
	logSum, n := 0.0, 0
	for _, r := range runs {
		ratios := []float64{r.SuccessfulRPS / r.OfferedRPS}
		for _, l := range []latency{r.ReadLatency, r.WriteLatency} {
			if l.P99 == nil {
				ratios = append(ratios, 0.0001)
			} else {
				ratios = append(ratios, targetMs/math.Max(*l.P99, 0.0001))
			}
		}
		for _, ratio := range ratios {
			logSum += math.Log(math.Max(math.Min(1, ratio), 1e-6))
			n++
		}
	}
	return math.Round(30*math.Exp(logSum/float64(n))*100) / 100
}

type runResult struct {
	Run         int              `json:"run"`
	SpeedPoints float64          `json:"speed_points"`
	Workloads   []workloadResult `json:"workloads"`
}

type pointsSummary struct {
	Runs  int     `json:"runs"`
	Mean  float64 `json:"mean"`
	Stdev float64 `json:"stdev"`
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
}

// workloadMean averages one workload's throughput and p99 across runs.
type workloadMean struct {
	Walls         int     `json:"walls"`
	SuccessfulRPS float64 `json:"successful_rps"`
	ReadP99       float64 `json:"read_p99_ms"`
	WriteP99      float64 `json:"write_p99_ms"`
}

type report struct {
	Config            map[string]any `json:"config"`
	Machine           string         `json:"machine"`
	CorrectnessOnly   bool           `json:"correctness_only"`
	Checks            []check        `json:"checks"`
	AllCorrect        bool           `json:"all_correct"`
	CorrectnessPoints int            `json:"correctness_points"`
	PerformancePoints *float64       `json:"performance_points"`
	TotalPoints       *float64       `json:"total_points"`
	Speed             *pointsSummary `json:"speed_summary"`
	WorkloadMeans     []workloadMean `json:"workload_means"`
	Runs              []runResult    `json:"runs"`
	GraderCPUSeconds  float64        `json:"grader_cpu_seconds"`
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func summarize(values []float64) pointsSummary {
	s := pointsSummary{Runs: len(values), Min: math.Inf(1), Max: math.Inf(-1)}
	for _, v := range values {
		s.Mean += v / float64(len(values))
		s.Min, s.Max = math.Min(s.Min, v), math.Max(s.Max, v)
	}
	for _, v := range values {
		s.Stdev += (v - s.Mean) * (v - s.Mean)
	}
	if len(values) > 1 {
		s.Stdev = math.Sqrt(s.Stdev / float64(len(values)-1))
	} else {
		s.Stdev = 0
	}
	s.Mean, s.Stdev, s.Min, s.Max = round2(s.Mean), round2(s.Stdev), round2(s.Min), round2(s.Max)
	return s
}

func main() {
	url := flag.String("url", "http://127.0.0.1:8765", "server base URL")
	rate := flag.Float64("rate", 10000, "offered requests per second in each load workload")
	duration := flag.Duration("duration", 3*time.Second, "length of each load workload")
	connections := flag.Int("connections", 64, "keep-alive connections, one request outstanding each")
	target := flag.Float64("p99-target-ms", 10, "p99 latency that earns full latency credit")
	runs := flag.Int("runs", 30, "scored load runs; speed points are their average")
	correctnessOnly := flag.Bool("correctness-only", false, "one short, low-rate load run; no speed score")
	output := flag.String("output", "", "also save the JSON report to this file")
	flag.Parse()
	// Snapshot decoding allocates ~50 KB per read; a lazier collector keeps the grader's own
	// GC work from adding latency that would be charged to the server.
	debug.SetGCPercent(400)
	if *rate <= 0 || *duration <= 0 || *connections < 1 || *target <= 0 || *runs < 1 {
		fmt.Fprintln(os.Stderr, "rate, duration, connections, runs and p99-target-ms must be positive")
		os.Exit(2)
	}
	if *correctnessOnly {
		*rate, *duration, *runs = math.Min(*rate, 2000), 500*time.Millisecond, 1
	}

	checks := functional(*url)
	mixed := check{Name: "mixed-load final state", Passed: true}
	fail := func(run int, message string) {
		mixed.Passed = false
		mixed.Error = truncate(fmt.Sprintf("run %d: %s", run, message), 1000)
	}
	if _, err := workload(*url, *rate, min(time.Second, *duration), *connections, 1); err != nil {
		fail(0, err.Error())
	}
	var results []runResult
	var speeds []float64
	for run := 1; run <= *runs && mixed.Passed; run++ {
		result := runResult{Run: run}
		for _, walls := range []int{1, 4} {
			r, err := workload(*url, *rate, *duration, *connections, walls)
			if err != nil {
				fail(run, err.Error())
				break
			}
			result.Workloads = append(result.Workloads, r)
			if !r.ExactFinalState || r.Errors > 0 {
				fail(run, strings.Join(append(r.ErrorExamples, "final state or version set differs from the oracle"), "; "))
			}
		}
		if len(result.Workloads) == 2 {
			result.SpeedPoints = speedPoints(result.Workloads, *target)
			speeds = append(speeds, result.SpeedPoints)
		}
		results = append(results, result)
		if !*correctnessOnly {
			fmt.Fprintf(os.Stderr, "run %d/%d: %.2f speed points\n", run, *runs, result.SpeedPoints)
		}
	}
	checks = append(checks, mixed)

	correct, points := true, 0
	for _, c := range checks {
		if c.Passed {
			points += 10
		} else {
			correct = false
		}
	}
	rep := report{
		Config: map[string]any{
			"suite_version": 5, "rate": *rate, "duration_seconds": duration.Seconds(),
			"connections": *connections, "p99_target_ms": *target, "runs": *runs,
		},
		Machine:           fmt.Sprintf("%s/%s (%d CPUs)", runtime.GOOS, runtime.GOARCH, runtime.NumCPU()),
		CorrectnessOnly:   *correctnessOnly,
		Checks:            checks,
		AllCorrect:        correct,
		CorrectnessPoints: points,
		WorkloadMeans:     []workloadMean{},
		Runs:              results,
	}
	if results == nil {
		rep.Runs = []runResult{}
	}
	for i, walls := range []int{1, 4} {
		m := workloadMean{Walls: walls}
		n := 0
		for _, r := range results {
			if len(r.Workloads) == 2 && r.Workloads[i].ReadLatency.P99 != nil && r.Workloads[i].WriteLatency.P99 != nil {
				m.SuccessfulRPS += r.Workloads[i].SuccessfulRPS
				m.ReadP99 += *r.Workloads[i].ReadLatency.P99
				m.WriteP99 += *r.Workloads[i].WriteLatency.P99
				n++
			}
		}
		if n > 0 {
			m.SuccessfulRPS, m.ReadP99, m.WriteP99 = round2(m.SuccessfulRPS/float64(n)), round2(m.ReadP99/float64(n)), round2(m.WriteP99/float64(n))
			rep.WorkloadMeans = append(rep.WorkloadMeans, m)
		}
	}
	if !*correctnessOnly {
		speed := 0.0
		if correct {
			summary := summarize(speeds)
			rep.Speed = &summary
			speed = summary.Mean
		}
		total := round2(float64(points) + speed)
		rep.PerformancePoints, rep.TotalPoints = &speed, &total
	}
	rep.GraderCPUSeconds = math.Round(cpuSeconds()*1000) / 1000
	out, _ := json.MarshalIndent(rep, "", "  ")
	if *output != "" {
		if err := os.WriteFile(*output, append(out, '\n'), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}
	fmt.Println(string(out))
	if !correct {
		os.Exit(1)
	}
}
