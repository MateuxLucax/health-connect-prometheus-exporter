package remotewrite

import (
	"reflect"
	"testing"
)

func TestGroupSeries(t *testing.T) {
	got := groupSeries([]Sample{
		{Metric: "b", Labels: map[string]string{"z": "1", "a": "2"}, Value: 1, TimestampMs: 30},
		{Metric: "b", Labels: map[string]string{"a": "2", "z": "1"}, Value: 2, TimestampMs: 10},
		{Metric: "b", Labels: map[string]string{"a": "2", "z": "1"}, Value: 3, TimestampMs: 30}, // duplicate ts: last wins
		{Metric: "a", Labels: map[string]string{"empty": ""}, Value: 4, TimestampMs: 5},
		{Metric: "a", Value: 5, TimestampMs: 6}, // same series as above
	})
	want := []series{
		{labels: []label{{"__name__", "a"}}, points: []point{{4, 5}, {5, 6}}},
		{labels: []label{{"__name__", "b"}, {"a", "2"}, {"z", "1"}}, points: []point{{2, 10}, {3, 30}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestSplitBatches(t *testing.T) {
	s := []series{
		{labels: []label{{"__name__", "a"}}, points: make([]point, 3)},
		{labels: []label{{"__name__", "b"}}, points: make([]point, 4)},
	}
	var sizes [][]int
	for _, b := range splitBatches(s, 3) {
		var n []int
		for _, x := range b {
			n = append(n, len(x.points))
		}
		sizes = append(sizes, n)
	}
	want := [][]int{{3}, {3}, {1}}
	if !reflect.DeepEqual(sizes, want) {
		t.Errorf("got %v, want %v", sizes, want)
	}
}
