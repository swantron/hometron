package main

import (
	"testing"
	"time"
)

func TestByDate(t *testing.T) {
	today := time.Now().Format("2006-01-02")
	old := time.Now().AddDate(0, 0, -(trafficDays - 1)).Format("2006-01-02")
	tooOld := time.Now().AddDate(0, 0, -trafficDays).Format("2006-01-02")
	in := []struct {
		d string
		v float64
	}{{today, 5}, {today, 2}, {old, 3}, {tooOld, 9}, {"garbage", 1}}
	got := byDate(len(in), func(i int) (string, float64) { return in[i].d, in[i].v })
	if len(got) != trafficDays || got[trafficDays-1] != 7 || got[0] != 3 || sum(got) != 10 {
		t.Fatalf("byDate = %v", got)
	}
}

func TestAddTo(t *testing.T) {
	a := addTo(nil, []float64{1, 2})
	a = addTo(a, []float64{3, 4})
	if a[0] != 4 || a[1] != 6 {
		t.Fatalf("addTo = %v", a)
	}
}
