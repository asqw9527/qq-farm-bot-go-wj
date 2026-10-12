package main

import (
	"reflect"
	"testing"

	"github.com/Aoluis1005/go-farm-bot/proto"
)

func unlockedLands(n int64) map[int64]*proto.LandInfo {
	m := make(map[int64]*proto.LandInfo, n)
	for i := int64(1); i <= n; i++ {
		m[i] = &proto.LandInfo{ID: i, Unlocked: true}
	}
	return m
}

func TestInferEmpty2x2IDsSixCol(t *testing.T) {
	lands := unlockedLands(24)
	cases := []struct {
		lid  int64
		want []int64
	}{
		{1, []int64{1, 2, 7, 8}},
		{5, []int64{5, 6, 11, 12}},
		{13, []int64{13, 14, 19, 20}},
		{17, []int64{17, 18, 23, 24}},
		{6, nil},
		{18, nil},
		{19, nil},
		{20, nil},
		{24, nil},
	}
	for _, c := range cases {
		got := inferEmpty2x2IDs(c.lid, lands)
		if !reflect.DeepEqual(got, c.want) {
			t.Fatalf("lid=%d got %v want %v", c.lid, got, c.want)
		}
	}
}

func TestInferEmpty2x2RejectsFourColGroup(t *testing.T) {
	got := inferEmpty2x2IDs(19, unlockedLands(24))
	if got != nil {
		t.Fatalf("4-col group [19 20 23 24] must not be inferred, got %v", got)
	}
}

func TestAnalyzeFarmLandsCollectsGoldUpgrade(t *testing.T) {
	lands := []*proto.LandInfo{
		{ID: 1, Unlocked: true, Level: 4, CouldUpgrade: true},
		{ID: 2, Unlocked: true, Level: 5, CouldUpgrade: false},
		{ID: 3, Unlocked: false, CouldUnlock: true, CouldUpgrade: false},
		{ID: 4, Unlocked: true, Level: 4, CouldUpgrade: true, MasterLandID: 1},
	}
	a := analyzeFarmLands(lands, 0)
	if !reflect.DeepEqual(a.couldUpgrade, []int64{1}) {
		t.Fatalf("couldUpgrade=%v want [1]", a.couldUpgrade)
	}
	if !reflect.DeepEqual(a.couldUnlock, []int64{3}) {
		t.Fatalf("couldUnlock=%v want [3]", a.couldUnlock)
	}
	if !reflect.DeepEqual(a.empty, []int64{1, 2}) {
		t.Fatalf("empty=%v want [1 2]", a.empty)
	}
}
