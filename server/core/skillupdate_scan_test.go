package core

import (
	"context"
	"errors"
	"testing"
	"time"
)

type skillUpdateTestLease struct {
	release func(context.Context) error
}

func (l skillUpdateTestLease) Renew(context.Context, time.Duration) (bool, error) {
	return true, nil
}
func (l skillUpdateTestLease) Release(ctx context.Context) error {
	if l.release != nil {
		return l.release(ctx)
	}
	return nil
}

type skillUpdateTestLeases struct {
	granted  bool
	err      error
	acquired int
	released int
}

func (l *skillUpdateTestLeases) Acquire(context.Context, string, time.Duration) (Lease, bool, error) {
	l.acquired++
	return skillUpdateTestLease{release: func(context.Context) error {
		l.released++
		return nil
	}}, l.granted, l.err
}

func TestScanSkillUpdatesLeaseRules(t *testing.T) {
	for _, tc := range []struct {
		name     string
		granted  bool
		err      error
		wantList int
	}{
		{name: "held elsewhere skips upstream work", granted: false},
		{name: "lease failure still scans", granted: false, err: errors.New("lease unavailable"), wantList: 1},
		{name: "acquired lease is released", granted: true, wantList: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leases := &skillUpdateTestLeases{granted: tc.granted, err: tc.err}
			lists := 0
			ScanSkillUpdates(context.Background(), SkillUpdateScanHooks{
				Leases: leases, LeaderKey: "skill-update-test",
				List: func(context.Context) ([]SkillUpdateItem, error) {
					lists++
					return nil, nil
				},
				Update: func(context.Context, SkillUpdateItem) (string, bool, error) {
					t.Fatal("empty list must not invoke update")
					return "", false, nil
				},
			})
			if lists != tc.wantList {
				t.Fatalf("List calls = %d, want %d", lists, tc.wantList)
			}
			wantReleased := 0
			if tc.granted && tc.err == nil {
				wantReleased = 1
			}
			if leases.released != wantReleased {
				t.Fatalf("Release calls = %d, want %d", leases.released, wantReleased)
			}
		})
	}
}

func TestScanSkillUpdatesContinuesAfterFailureAndKeepsOpaqueItem(t *testing.T) {
	type privateSkill struct{ checksum string }
	original := privateSkill{checksum: "old"}
	items := []SkillUpdateItem{
		{ID: "bad", Value: original},
		{ID: "good", Version: "v1", Value: original},
	}
	updates := 0
	ScanSkillUpdates(context.Background(), SkillUpdateScanHooks{
		List: func(context.Context) ([]SkillUpdateItem, error) { return items, nil },
		Update: func(_ context.Context, item SkillUpdateItem) (string, bool, error) {
			updates++
			got, ok := item.Value.(privateSkill)
			if !ok || got != original {
				t.Fatalf("opaque downstream skill changed: %#v", item.Value)
			}
			if item.ID == "bad" {
				return "", false, errors.New("upstream denied")
			}
			return "v2", true, nil
		},
	})
	if updates != 2 {
		t.Fatalf("Update calls = %d, want both items attempted", updates)
	}
}

func TestScanSkillUpdatesBoundsBatch(t *testing.T) {
	updates := 0
	ScanSkillUpdates(context.Background(), SkillUpdateScanHooks{
		MaxPerScan: 2,
		List: func(context.Context) ([]SkillUpdateItem, error) {
			return []SkillUpdateItem{{ID: "a"}, {ID: "b"}, {ID: "c"}}, nil
		},
		Update: func(context.Context, SkillUpdateItem) (string, bool, error) {
			updates++
			return "", false, nil
		},
	})
	if updates != 2 {
		t.Fatalf("Update calls = %d, want 2", updates)
	}
}
