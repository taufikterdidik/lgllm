package server

import (
	"testing"
	"time"
)

func TestComboStatusTracksLastMemberAndErrors(t *testing.T) {
	status := newComboStatusStore()
	t0 := time.Now()
	status.Failed("combo", "a/model", 500, "conn-a", "boom", t0)
	status.Used("combo", "b/model", t0.Add(time.Second))

	last, used, errs := status.Snapshot("combo")
	if last != "b/model" {
		t.Errorf("last member = %q", last)
	}
	if !used.Equal(t0.Add(time.Second)) {
		t.Errorf("last used = %v", used)
	}
	if errs["a/model"].Message != "boom" || errs["a/model"].Status != 500 ||
		errs["a/model"].Connection != "conn-a" {
		t.Errorf("member error = %+v", errs["a/model"])
	}

	// A success clears that member's old error.
	status.Used("combo", "a/model", t0.Add(2*time.Second))
	_, _, errs = status.Snapshot("combo")
	if _, ok := errs["a/model"]; ok {
		t.Errorf("successful member still has an error: %+v", errs)
	}

	// An out-of-order failure must not replace a newer one.
	status.Failed("combo", "a/model", 502, "", "new", t0.Add(3*time.Second))
	status.Failed("combo", "a/model", 500, "", "old", t0.Add(2*time.Second))
	_, _, errs = status.Snapshot("combo")
	if errs["a/model"].Message != "new" {
		t.Errorf("older error replaced the newer one: %+v", errs["a/model"])
	}
}
