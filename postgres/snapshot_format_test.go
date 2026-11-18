package postgres

import "testing"

func TestSnapshotRequiresPositiveTransactionIDs(t *testing.T) {
	for _, s := range []string{"0:0:", "0:1:"} {
		if validSnapshot(s) {
			t.Errorf("accepted invalid snapshot %q", s)
		}
	}
	if !validSnapshot("1:1:") {
		t.Fatal("rejected minimum valid snapshot")
	}
}
