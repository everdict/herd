package auth

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// The counters of the allowances ADR 0001 A-60 keeps, served in the Prometheus text format (Meter.Handler).
const (
	// MetricBreakGlassCalls counts the calls an HS256 bearer authenticated, by subject.
	MetricBreakGlassCalls = "herd_auth_breakglass_calls_total"
	// MetricUnownedActorWrites counts the writes on actors with no creator recorded, by allowance and caller.
	MetricUnownedActorWrites = "herd_auth_unowned_actor_writes_total"
	// MetricPolicyStale is 1 while the policy file's last change did not parse (the last good policy is in force).
	MetricPolicyStale = "herd_auth_policy_stale"
)

var metricHelp = map[string]string{
	MetricBreakGlassCalls:    "Calls authenticated by an HS256 shared-key bearer (break-glass, ADR 0001 A-60), by subject.",
	MetricUnownedActorWrites: "Writes on actors with no creator recorded, allowed to an operator or to break-glass (ADR 0001 A-60).",
	MetricPolicyStale:        "1 while the SPIFFE policy file's last change did not parse and the last good policy is in force.",
}

// gauges are the metrics of metricHelp that are gauges, not counters.
var gauges = map[string]bool{MetricPolicyStale: true}

// Meter is a set of counters keyed by name and labels. A nil Meter counts nothing.
type Meter struct {
	// Now is the clock the log limit reads (default time.Now).
	Now  func() time.Time
	mu   sync.Mutex
	c    map[string]map[string]uint64 // name → rendered labels → count
	logs map[string]*logWindow        // Note's limit per key
	g    map[string]float64           // gauges by name
}

// NewMeter returns an empty meter; the A-60 counters are listed (at no samples) from the start.
func NewMeter() *Meter { return &Meter{c: map[string]map[string]uint64{}} }

// labels renders label pairs (k1, v1, k2, v2, …) as Prometheus does.
func labels(kv []string) string {
	if len(kv) == 0 {
		return ""
	}
	var parts []string
	for i := 0; i+1 < len(kv); i += 2 {
		v := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(kv[i+1])
		parts = append(parts, fmt.Sprintf("%s=%q", kv[i], v))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// Inc adds one to the counter name with the label pairs kv.
func (m *Meter) Inc(name string, kv ...string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.c[name] == nil {
		m.c[name] = map[string]uint64{}
	}
	m.c[name][labels(kv)]++
}

// Value is the counter name with the label pairs kv.
func (m *Meter) Value(name string, kv ...string) uint64 {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.c[name][labels(kv)]
}

// WriteText writes every counter in the Prometheus text exposition format.
func (m *Meter) WriteText(w io.Writer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(metricHelp))
	for n := range metricHelp {
		names = append(names, n)
	}
	for n := range m.c {
		if _, ok := metricHelp[n]; !ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		if h := metricHelp[n]; h != "" {
			if _, err := fmt.Fprintf(w, "# HELP %s %s\n", n, h); err != nil {
				return err
			}
		}
		if gauges[n] {
			if _, err := fmt.Fprintf(w, "# TYPE %s gauge\n%s %v\n", n, n, m.g[n]); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(w, "# TYPE %s counter\n", n); err != nil {
			return err
		}
		ls := make([]string, 0, len(m.c[n]))
		for l := range m.c[n] {
			ls = append(ls, l)
		}
		sort.Strings(ls)
		for _, l := range ls {
			if _, err := fmt.Fprintf(w, "%s%s %d\n", n, l, m.c[n][l]); err != nil {
				return err
			}
		}
	}
	return nil
}

// Handler serves the meter at any path, in the Prometheus text format.
func (m *Meter) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_ = m.WriteText(w)
	})
}

// LogEvery is how often Note logs one summary line per key: a polling caller is counted exactly on the meter, while
// the log carries its first call and then a sum per minute (ADR 0001 A-60).
const LogEvery = time.Minute

// logWindow is one key's log limit: when its last line was written and how many events came since.
type logWindow struct {
	since time.Time
	n     int
}

// Note logs an event of key without flooding the log: the key's first event logs first at once; later ones are
// summed, and the first event a LogEvery or more after the key's last line logs summary(n, elapsed), n being the
// events since that line, this one included. Counting stays the caller's (Inc), exact per event. A key whose events
// stop leaves its last sum unlogged; the counter has it. A nil Meter logs every event's first line.
func (m *Meter) Note(key, first string, summary func(n int, elapsed time.Duration) string) {
	if m == nil {
		log.Print(first)
		return
	}
	m.mu.Lock()
	now := time.Now()
	if m.Now != nil {
		now = m.Now()
	}
	if m.logs == nil {
		m.logs = map[string]*logWindow{}
	}
	w, seen := m.logs[key]
	line := ""
	switch {
	case !seen:
		m.logs[key] = &logWindow{since: now}
		line = first
	default:
		w.n++
		if d := now.Sub(w.since); d >= LogEvery {
			line = summary(w.n, d)
			w.since, w.n = now, 0
		}
	}
	m.mu.Unlock()
	if line != "" {
		log.Print(line)
	}
}

// Seconds renders an elapsed time for a summary line ("61s").
func Seconds(d time.Duration) string { return fmt.Sprintf("%ds", int(d.Seconds())) }

// SetGauge sets the gauge name.
func (m *Meter) SetGauge(name string, v float64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.g == nil {
		m.g = map[string]float64{}
	}
	m.g[name] = v
}

// Gauge is the gauge name (0 if never set).
func (m *Meter) Gauge(name string) float64 {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.g[name]
}
