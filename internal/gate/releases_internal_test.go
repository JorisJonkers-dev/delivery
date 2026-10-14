package gate

import (
	"context"
	"testing"
	"time"
)

// counted is a cluster with no gated Application that counts how often it is asked for them.
type counted struct {
	Cluster
	asked int
}

func (c *counted) Applications(context.Context) ([]Gated, error) {
	c.asked++
	return nil, nil
}

func TestTheReleasesAreReadAtMostOncePerPeriod(t *testing.T) {
	c := &counted{}
	g := New(c)
	at := time.Date(2026, 10, 14, 21, 0, 0, 0, time.UTC)
	g.served.now = func() time.Time { return at }
	for range 50 {
		if _, err := g.servedReleases(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	at = at.Add(servedFor - time.Nanosecond)
	if _, err := g.servedReleases(t.Context()); err != nil || c.asked != 1 {
		t.Fatalf("within the period, %d reads, %v", c.asked, err)
	}
	at = at.Add(time.Nanosecond)
	if _, err := g.servedReleases(t.Context()); err != nil || c.asked != 2 {
		t.Fatalf("once the period is over, %d reads, %v", c.asked, err)
	}
}
