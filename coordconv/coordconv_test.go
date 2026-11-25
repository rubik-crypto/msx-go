package coordconv

import (
	"math"
	"testing"
)

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func TestWGS84ToGCJ02KnownVector(t *testing.T) {
	// 公共测试向量：WGS-84 (39.915, 116.404) → GCJ-02。
	got := WGS84ToGCJ02(Point{Lat: 39.915, Lng: 116.404})
	want := Point{Lat: 39.91640428150164, Lng: 116.41024449916938}
	if !almostEqual(got.Lat, want.Lat) || !almostEqual(got.Lng, want.Lng) {
		t.Fatalf("WGS84ToGCJ02 = %+v，想要 %+v", got, want)
	}
}

func TestGCJ02ToBD09KnownVector(t *testing.T) {
	// 公共测试向量：GCJ-02 (39.915, 116.404) → BD-09。
	got := GCJ02ToBD09(Point{Lat: 39.915, Lng: 116.404})
	want := Point{Lat: 39.92133699351021, Lng: 116.41036949371029}
	if !almostEqual(got.Lat, want.Lat) || !almostEqual(got.Lng, want.Lng) {
		t.Fatalf("GCJ02ToBD09 = %+v，想要 %+v", got, want)
	}
}

func TestRoundTrip(t *testing.T) {
	points := []Point{
		{Lat: 39.9042, Lng: 116.4074},
		{Lat: 31.2304, Lng: 121.4737},
		{Lat: 22.5431, Lng: 114.0579},
		{Lat: 43.8256, Lng: 87.6168},
	}
	for _, p := range points {
		w := GCJ02ToWGS84(WGS84ToGCJ02(p))
		if math.Abs(w.Lat-p.Lat) > 1e-6 || math.Abs(w.Lng-p.Lng) > 1e-6 {
			t.Fatalf("%+v 往返误差太大：%+v", p, w)
		}
		b := BD09ToGCJ02(GCJ02ToBD09(p))
		if math.Abs(b.Lat-p.Lat) > 1e-6 || math.Abs(b.Lng-p.Lng) > 1e-6 {
			t.Fatalf("%+v 百度坐标往返误差太大：%+v", p, b)
		}
	}
}

func TestOutsideOffsetArea(t *testing.T) {
	points := []Point{
		{Lat: 48.8566, Lng: 2.3522},    // 巴黎
		{Lat: -33.8688, Lng: 151.2093}, // 悉尼
		{Lat: 40.7128, Lng: -74.006},   // 纽约
	}
	for _, p := range points {
		if InOffsetArea(p) {
			t.Fatalf("%+v 不应在偏移范围内", p)
		}
		if got := WGS84ToGCJ02(p); got != p {
			t.Fatalf("偏移范围外的坐标不该变：%+v → %+v", p, got)
		}
		if got := GCJ02ToWGS84(p); got != p {
			t.Fatalf("偏移范围外的坐标不该变：%+v → %+v", p, got)
		}
	}
	if !InOffsetArea(Point{Lat: 39.9042, Lng: 116.4074}) {
		t.Fatal("北京应该在偏移范围内")
	}
}

func TestBaiduHelpers(t *testing.T) {
	p := Point{Lat: 30.5728, Lng: 104.0668}
	if got := BD09ToWGS84(WGS84ToBD09(p)); math.Abs(got.Lat-p.Lat) > 1e-6 || math.Abs(got.Lng-p.Lng) > 1e-6 {
		t.Fatalf("WGS-84 与 BD-09 往返误差太大：%+v", got)
	}
}
