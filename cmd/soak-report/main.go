// Command soak-report judges a long load test (the 10,000 TPS 24-hour soak,
// docs/soak-test.md) from Prometheus: nothing should grow that ought to
// stay flat, and nothing should restart.
//
//   - memory (RSS) and goroutines of every service: the hour after warm-up
//     against the last hour;
//   - messages in flight (accepted, not yet final): same comparison, so a
//     backlog building up slowly shows even when throughput keeps up;
//   - no service restarted; no submit refused; no history row dropped.
//
// It exits 0 when every check passes, 2 when one fails, 1 on errors.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

func main() {
	prom := flag.String("prometheus", "http://localhost:9090", "Prometheus URL")
	startS := flag.String("start", "", "soak start, RFC 3339 (required)")
	endS := flag.String("end", "", "soak end, RFC 3339 (default now)")
	warmup := flag.Duration("warmup", time.Hour, "ignore this long after the start (caches filling, JIT of the data stores)")
	window := flag.Duration("window", time.Hour, "averaging window for the early and late samples")
	memGrowth := flag.Float64("max-memory-growth", 0.25, "fail if a service's RSS grows more than this fraction (and by more than 64 MiB)")
	gorGrowth := flag.Float64("max-goroutine-growth", 0.5, "fail if a service's goroutines grow more than this fraction (and by more than 200)")
	inflightGrowth := flag.Float64("max-inflight-growth", 0.5, "fail if messages in flight grow more than this fraction (and by more than 10,000)")
	maxRefused := flag.Float64("max-refused", 0, "fail if more submits than this were refused")
	reportPath := flag.String("report", "", "also write the result as JSON here")
	flag.Parse()

	start, err := time.Parse(time.RFC3339, *startS)
	if err != nil {
		fail(fmt.Errorf("-start must be RFC 3339: %w", err))
	}
	end := time.Now().UTC()
	if *endS != "" {
		if end, err = time.Parse(time.RFC3339, *endS); err != nil {
			fail(fmt.Errorf("-end must be RFC 3339: %w", err))
		}
	}
	early := start.Add(*warmup + *window)
	if !early.Before(end) {
		fail(fmt.Errorf("the run (%s) is shorter than warm-up + window (%s)", end.Sub(start), *warmup+*window))
	}
	c := &client{base: strings.TrimRight(*prom, "/"), http: &http.Client{Timeout: 30 * time.Second}}
	r := evaluate(c, early, end, end.Sub(start), *window, limits{*memGrowth, *gorGrowth, *inflightGrowth, *maxRefused})
	for _, l := range r.Lines {
		fmt.Println(l)
	}
	if *reportPath != "" {
		b, _ := json.MarshalIndent(r, "", "  ")
		if err := os.WriteFile(*reportPath, b, 0o644); err != nil {
			fail(err)
		}
	}
	if r.Pass {
		fmt.Println("RESULT: PASS")
		return
	}
	fmt.Println("RESULT: FAIL")
	os.Exit(2)
}

type limits struct {
	mem, goroutines, inflight, refused float64
}

type result struct {
	Pass     bool     `json:"pass"`
	Lines    []string `json:"lines"`
	Failures []string `json:"failures,omitempty"`
}

// evaluate runs every check. early and end are the times of the two
// averaged samples; run is the whole soak (for counters).
func evaluate(c querier, early, end time.Time, run, window time.Duration, lim limits) result {
	var r result
	check := func(name string, ok bool, detail string) {
		mark := "PASS"
		if !ok {
			mark = "FAIL"
			r.Failures = append(r.Failures, name+": "+detail)
		}
		r.Lines = append(r.Lines, fmt.Sprintf("%-50s %s  %s", name, mark, detail))
	}
	w := promDur(window)
	// growth compares a query averaged over the window after warm-up with
	// the same over the last window. q has %[1]s for the window.
	growth := func(name, q string, maxFrac, minAbs float64, unit func(float64) string) {
		q = fmt.Sprintf(q, w)
		a, err1 := c.vector(q, early)
		b, err2 := c.vector(q, end)
		if err1 != nil || err2 != nil {
			check(name, false, fmt.Sprint(err1, err2))
			return
		}
		var bad, all []string
		for _, k := range sortedKeys(b) {
			before, ok := a[k]
			if !ok || before == 0 {
				continue
			}
			after := b[k]
			g := (after - before) / before
			line := fmt.Sprintf("%s %s -> %s (%+.0f%%)", k, unit(before), unit(after), 100*g)
			all = append(all, line)
			if g > maxFrac && after-before > minAbs {
				bad = append(bad, line)
			}
		}
		if len(all) == 0 {
			check(name, false, "no samples (is Prometheus scraping the services?)")
			return
		}
		if len(bad) > 0 {
			check(name, false, strings.Join(bad, "; "))
			return
		}
		check(name, true, fmt.Sprintf("%d series flat (largest: %s)", len(all), largest(all)))
	}
	mib := func(v float64) string { return fmt.Sprintf("%.0fMiB", v/(1<<20)) }
	plain := func(v float64) string { return strconv.FormatFloat(v, 'f', 0, 64) }
	growth("memory (RSS) per service", `avg_over_time(process_resident_memory_bytes{job="platform"}[%[1]s])`, lim.mem, 64<<20, mib)
	growth("goroutines per service", `avg_over_time(go_goroutines{job="platform"}[%[1]s])`, lim.goroutines, 200, plain)
	growth("messages in flight (accepted, not final)",
		`label_replace(avg_over_time((sum(sms_accepted_total) - sum(sms_final_total))[%[1]s:1m]), "instance", "platform", "", "")`,
		lim.inflight, 10000, plain)

	full := promDur(run)
	counter := func(name, q string, limit float64) {
		v, err := c.scalar(fmt.Sprintf(q, full), end)
		if err != nil {
			check(name, false, err.Error())
			return
		}
		check(name, v <= limit, fmt.Sprintf("%.0f (limit %.0f)", v, limit))
	}
	counter("service restarts", `sum(changes(process_start_time_seconds{job="platform"}[%s]))`, 0)
	counter("submits refused", `sum(increase(sms_refused_total[%s])) or vector(0)`, lim.refused)
	counter("history rows dropped", `sum(increase(sms_records_dropped_total[%s])) or vector(0)`, 0)
	if v, err := c.scalar(fmt.Sprintf(`sum(increase(sms_consumer_batch_retries_total[%s])) or vector(0)`, full), end); err == nil {
		r.Lines = append(r.Lines, fmt.Sprintf("%-50s info  %.0f (a dependency failed briefly)", "consumer batches retried", v))
	}
	if v, err := c.scalar(fmt.Sprintf(`sum(increase(sms_accepted_total[%s]))`, full), end); err == nil {
		r.Lines = append(r.Lines, fmt.Sprintf("%-50s info  %.0f (%.0f per second)", "messages accepted", v, v/run.Seconds()))
	}
	r.Pass = len(r.Failures) == 0
	return r
}

func largest(lines []string) string {
	best, bestG := "", -1e18
	for _, l := range lines {
		i := strings.LastIndex(l, "(")
		g, _ := strconv.ParseFloat(strings.TrimSuffix(strings.TrimPrefix(l[i+1:], "+"), "%)"), 64)
		if g > bestG {
			best, bestG = l, g
		}
	}
	return best
}

func promDur(d time.Duration) string { return fmt.Sprintf("%ds", int64(d.Seconds())) }

func sortedKeys(m map[string]float64) []string {
	k := make([]string, 0, len(m))
	for s := range m {
		k = append(k, s)
	}
	sort.Strings(k)
	return k
}

type querier interface {
	vector(q string, at time.Time) (map[string]float64, error)
	scalar(q string, at time.Time) (float64, error)
}

type client struct {
	base string
	http *http.Client
}

// vector runs an instant query and keys the samples by instance (or by
// all their labels when there is no instance label).
func (c *client) vector(q string, at time.Time) (map[string]float64, error) {
	u := c.base + "/api/v1/query?" + url.Values{"query": {q}, "time": {at.UTC().Format(time.RFC3339)}}.Encode()
	resp, err := c.http.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var body struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  [2]any            `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("prometheus: %w", err)
	}
	if body.Status != "success" {
		return nil, fmt.Errorf("prometheus: %s", body.Error)
	}
	out := map[string]float64{}
	for _, s := range body.Data.Result {
		v, _ := strconv.ParseFloat(fmt.Sprint(s.Value[1]), 64)
		key := s.Metric["instance"]
		if key == "" {
			var parts []string
			for k, val := range s.Metric {
				parts = append(parts, k+"="+val)
			}
			sort.Strings(parts)
			key = strings.Join(parts, ",")
		}
		out[key] = v
	}
	return out, nil
}

func (c *client) scalar(q string, at time.Time) (float64, error) {
	m, err := c.vector(q, at)
	if err != nil {
		return 0, err
	}
	var sum float64
	for _, v := range m {
		sum += v
	}
	return sum, nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "soak-report:", err)
	os.Exit(1)
}
