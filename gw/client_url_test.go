package gw

import "testing"

func TestBuildGatewayURLEncodesCode(t *testing.T) {
	got, err := buildGatewayURL("wss://gate-obt.nqf.qq.com/prod/ws", "wx", "iOS", "1.13.2.10_20260723", "abc+/=def")
	if err != nil {
		t.Fatal(err)
	}
	want := "wss://gate-obt.nqf.qq.com/prod/ws?platform=wx&os=iOS&ver=1.13.2.10_20260723&code=abc%2B%2F%3Ddef&openID="
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}
