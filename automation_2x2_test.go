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

func TestInferEmpty2x2IDsFourCol(t *testing.T) {
	lands := unlockedLands(24)
	cases := []struct {
		lid  int64
		want []int64
	}{
		{5, []int64{5, 6, 1, 2}},
		{6, []int64{6, 7, 2, 3}},
		{7, []int64{7, 8, 3, 4}},
		{9, []int64{9, 10, 5, 6}},
		{21, []int64{21, 22, 17, 18}},
		{23, []int64{23, 24, 19, 20}},
		{1, nil},
		{2, nil},
		{4, nil},
		{8, nil},
		{24, nil},
	}
	for _, c := range cases {
		got := inferEmpty2x2IDs(c.lid, lands)
		if !reflect.DeepEqual(got, c.want) {
			t.Fatalf("lid=%d got %v want %v", c.lid, got, c.want)
		}
	}
}

func TestInferEmpty2x2RejectsSixColGroup(t *testing.T) {
	got := inferEmpty2x2IDs(1, unlockedLands(24))
	if got != nil {
		t.Fatalf("6-col group [1 2 7 8] must not be inferred, got %v", got)
	}
}

func TestBuild2x2LandGroupsFourCol(t *testing.T) {
	groups := build2x2LandGroups(unlockedLands(24))
	if len(groups) != 15 {
		t.Fatalf("group count=%d want 15", len(groups))
	}
	if groups[0].master != 5 || !reflect.DeepEqual(groups[0].ids, []int64{5, 6, 1, 2}) {
		t.Fatalf("first group=%+v want master=5 ids=[5 6 1 2]", groups[0])
	}
	last := groups[len(groups)-1]
	if last.master != 23 || !reflect.DeepEqual(last.ids, []int64{23, 24, 19, 20}) {
		t.Fatalf("last group=%+v want master=23 ids=[23 24 19 20]", last)
	}
}

func TestSelectReady2x2GroupsSkipsOccupied(t *testing.T) {
	lands := unlockedLands(24)
	lands[2].Plant = &proto.PlantInfo{Phases: []*proto.PlantPhaseInfo{{Phase: proto.PhaseMature, BeginTime: 1}}}
	ready, waiting := selectReady2x2Groups(build2x2LandGroups(lands), lands, nil)
	for _, g := range ready {
		for _, id := range g.ids {
			if id == 2 {
				t.Fatalf("ready group %v includes occupied land 2", g.ids)
			}
		}
	}
	if len(waiting) == 0 {
		t.Fatal("want waiting groups overlapping occupied land 2")
	}
}

func TestAnalyzeFarmLandsCollectsGoldUpgrade(t *testing.T) {
	lands := []*proto.LandInfo{
		{ID: 1, Unlocked: true, Level: 4, CouldUpgrade: true},
		{ID: 2, Unlocked: true, Level: 5, CouldUpgrade: false},
		{ID: 3, Unlocked: false, CouldUnlock: true, CouldUpgrade: false},
		{ID: 4, Unlocked: true, Level: 4, CouldUpgrade: true, MasterLandID: 1},
		{ID: 5, Unlocked: true, Level: 4, CouldUpgrade: true, Plant: &proto.PlantInfo{Phases: []*proto.PlantPhaseInfo{{Phase: 3, BeginTime: 1}}}},
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
