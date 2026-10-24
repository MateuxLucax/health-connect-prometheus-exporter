package remotewrite

import (
	"sort"
	"strings"
)

// Sample is one validated sample to write: a metric name, its labels, a value
// and the sample's own timestamp in Unix milliseconds. It must already satisfy
// the Prometheus naming rules: Metric is a valid metric name and every label
// name is a valid label name that does not start with "__". This package does
// not check them; callers validate first (the ingest package does).
type Sample struct {
	Metric      string
	Labels      map[string]string
	Value       float64
	TimestampMs int64
}

type label struct{ name, value string }

type point struct {
	value float64
	tsMs  int64
}

type series struct {
	labels []label // sorted by name, including __name__
	points []point // sorted by timestamp, unique timestamps
}

// groupSeries groups samples into series by metric name and labels. Within a
// series, points are sorted by timestamp and for duplicate timestamps the last
// sample in the request wins. Labels with an empty value are dropped, as an
// empty value means "no label" in Prometheus.
func groupSeries(samples []Sample) []series {
	type acc struct {
		labels []label
		byTs   map[int64]float64
	}
	groups := map[string]*acc{}
	for _, s := range samples {
		labels := []label{{"__name__", s.Metric}}
		for n, v := range s.Labels {
			if v != "" {
				labels = append(labels, label{n, v})
			}
		}
		sort.Slice(labels, func(i, j int) bool { return labels[i].name < labels[j].name })
		var key strings.Builder
		for _, l := range labels {
			key.WriteString(l.name + "\xff" + l.value + "\xff")
		}
		g := groups[key.String()]
		if g == nil {
			g = &acc{labels: labels, byTs: map[int64]float64{}}
			groups[key.String()] = g
		}
		g.byTs[s.TimestampMs] = s.Value
	}

	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]series, 0, len(keys))
	for _, k := range keys {
		g := groups[k]
		points := make([]point, 0, len(g.byTs))
		for ts, v := range g.byTs {
			points = append(points, point{v, ts})
		}
		sort.Slice(points, func(i, j int) bool { return points[i].tsMs < points[j].tsMs })
		out = append(out, series{g.labels, points})
	}
	return out
}

// splitBatches splits series so that no batch holds more than max points.
func splitBatches(all []series, max int) [][]series {
	var out [][]series
	var cur []series
	n := 0
	for _, s := range all {
		for len(s.points) > 0 {
			take := min(max-n, len(s.points))
			cur = append(cur, series{s.labels, s.points[:take]})
			s.points = s.points[take:]
			n += take
			if n == max {
				out = append(out, cur)
				cur, n = nil, 0
			}
		}
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}
